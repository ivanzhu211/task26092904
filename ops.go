package approval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// createCase 在事务内创建案件、节点实例与第一阶段待办，返回案件 ID。
func createCase(ctx context.Context, tx *sql.Tx, reqID string, tpl Template, applicant string) (string, error) {
	sum := sha256.Sum256([]byte("case|" + reqID))
	caseID := "C" + hex.EncodeToString(sum[:])[:24]

	tplJSON, err := json.Marshal(tpl)
	if err != nil {
		return "", err
	}
	ts := formatTime(now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cases(id, template_id, template_name, template_json, applicant, status, current_stage, return_stage, round, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,0,-1,0,?,?)`,
		caseID, tpl.ID, tpl.Name, string(tplJSON), applicant, string(CaseRunning), ts, ts); err != nil {
		return "", err
	}
	for si, stage := range tpl.Stages {
		for _, n := range stage.Nodes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO nodes(case_id, node_id, stage, name, mode, status, round) VALUES(?,?,?,?,?,?,0)`,
				caseID, n.ID, si, n.Name, string(n.Mode), string(NodePending)); err != nil {
				return "", err
			}
			for _, a := range n.Assignees {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO node_assignees(case_id, node_id, assignee) VALUES(?,?,?)`,
					caseID, n.ID, a); err != nil {
					return "", err
				}
			}
		}
	}
	if err := spawnStageTasks(ctx, tx, caseID, 0, 0, ts); err != nil {
		return "", err
	}
	if err := writeAudit(ctx, tx, caseID, reqID, ActionCreate, applicant, map[string]string{
		"template_id":   tpl.ID,
		"applicant":     applicant,
		"template_json": string(tplJSON),
	}); err != nil {
		return "", err
	}
	return caseID, nil
}

// spawnStageTasks 为某阶段所有节点的当前审批人生成一轮有效待办。
func spawnStageTasks(ctx context.Context, tx *sql.Tx, caseID string, stage, round int, ts string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.node_id, a.assignee FROM nodes n
		 JOIN node_assignees a ON a.case_id = n.case_id AND a.node_id = n.node_id
		 WHERE n.case_id = ? AND n.stage = ?`, caseID, stage)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pair struct{ node, assignee string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.node, &p.assignee); err != nil {
			return err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks(case_id, node_id, stage, round, assignee, status, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			caseID, p.node, stage, round, p.assignee, string(TaskPending), ts, ts); err != nil {
			return err
		}
	}
	return nil
}

// decide 处理审批人对一条待办的同意/驳回/退回。
func decide(ctx context.Context, tx *sql.Tx, reqID, caseID string, taskID int64, actor string, action Action) error {
	if actor == "" {
		return fmt.Errorf("%w: actor is empty", ErrInvalidArgument)
	}
	var (
		taskCaseID, nodeID, assignee, taskStatus string
		stage, round                             int
	)
	err := tx.QueryRowContext(ctx,
		`SELECT case_id, node_id, stage, round, assignee, status FROM tasks WHERE id = ?`, taskID).
		Scan(&taskCaseID, &nodeID, &stage, &round, &assignee, &taskStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %d", ErrTaskNotFound, taskID)
	}
	if err != nil {
		return err
	}
	if taskCaseID != caseID {
		return fmt.Errorf("%w: task %d does not belong to case %s", ErrTaskNotFound, taskID, caseID)
	}
	if taskStatus != string(TaskPending) {
		return fmt.Errorf("%w: task %d is %s", ErrTaskNotPending, taskID, taskStatus)
	}
	if assignee != actor {
		return fmt.Errorf("%w: task %d belongs to %s, not %s", ErrTaskNotOwned, taskID, assignee, actor)
	}

	c, err := loadCase(ctx, tx, caseID)
	if err != nil {
		return err
	}
	switch c.status {
	case CaseApproved, CaseRejected, CaseWithdrawn:
		return fmt.Errorf("%w: case %s is %s", ErrCaseClosed, caseID, c.status)
	case CasePendingRevision:
		return fmt.Errorf("%w: case %s is waiting for resubmission", ErrCaseNotRunning, caseID)
	}
	if stage != c.currentStage || round != c.round {
		// 过期轮次的待办（理论上应已作废，双保险）。
		return fmt.Errorf("%w: task %d is from a stale round", ErrTaskNotPending, taskID)
	}

	ts := formatTime(now())
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, result = ?, updated_at = ? WHERE id = ?`,
		string(TaskDone), string(action), ts, taskID); err != nil {
		return err
	}

	switch action {
	case ActionReject:
		if err := voidPendingTasks(ctx, tx, caseID, ts); err != nil {
			return err
		}
		if err := voidPendingNodes(ctx, tx, caseID); err != nil {
			return err
		}
		if err := setCaseStatus(ctx, tx, caseID, CaseRejected, c.currentStage, c.returnStage, c.round, ts); err != nil {
			return err
		}
	case ActionReturn:
		if err := voidPendingTasks(ctx, tx, caseID, ts); err != nil {
			return err
		}
		if err := setCaseStatus(ctx, tx, caseID, CasePendingRevision, c.currentStage, c.currentStage, c.round, ts); err != nil {
			return err
		}
	case ActionApprove:
		if err := afterApprove(ctx, tx, c, nodeID, ts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported decide action %q", ErrInvalidArgument, action)
	}

	return writeAudit(ctx, tx, caseID, reqID, action, actor, map[string]string{
		"task_id": fmt.Sprint(taskID),
		"node_id": nodeID,
	})
}

// caseRow 是 cases 表的内存表示。
type caseRow struct {
	id           string
	applicant    string
	status       CaseStatus
	currentStage int
	returnStage  int
	round        int
}

func loadCase(ctx context.Context, tx *sql.Tx, caseID string) (*caseRow, error) {
	var c caseRow
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT id, applicant, status, current_stage, return_stage, round FROM cases WHERE id = ?`, caseID).
		Scan(&c.id, &c.applicant, &status, &c.currentStage, &c.returnStage, &c.round)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return nil, err
	}
	c.status = CaseStatus(status)
	return &c, nil
}

// voidPendingTasks 作废案件所有有效待办。
func voidPendingTasks(ctx context.Context, tx *sql.Tx, caseID, ts string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE case_id = ? AND status = ?`,
		string(TaskVoided), ts, caseID, string(TaskPending))
	return err
}

// voidPendingNodes 作废案件所有未完成节点。
func voidPendingNodes(ctx context.Context, tx *sql.Tx, caseID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE nodes SET status = ? WHERE case_id = ? AND status = ?`,
		string(NodeVoided), caseID, string(NodePending))
	return err
}

func setCaseStatus(ctx context.Context, tx *sql.Tx, caseID string, status CaseStatus, currentStage, returnStage, round int, ts string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE cases SET status = ?, current_stage = ?, return_stage = ?, round = ?, updated_at = ? WHERE id = ?`,
		string(status), currentStage, returnStage, round, ts, caseID)
	return err
}

// afterApprove 在一次同意之后推进节点/阶段/案件状态。
func afterApprove(ctx context.Context, tx *sql.Tx, c *caseRow, nodeID, ts string) error {
	var mode string
	if err := tx.QueryRowContext(ctx,
		`SELECT mode FROM nodes WHERE case_id = ? AND node_id = ?`, c.id, nodeID).Scan(&mode); err != nil {
		return err
	}
	if mode == string(ModeAny) {
		// 或签：一人同意即通过，其余待办作废。
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, updated_at = ?
			 WHERE case_id = ? AND node_id = ? AND round = ? AND status = ?`,
			string(TaskVoided), ts, c.id, nodeID, c.round, string(TaskPending)); err != nil {
			return err
		}
	} else {
		// 会签：该节点本轮还有有效待办就继续等。
		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tasks WHERE case_id = ? AND node_id = ? AND round = ? AND status = ?`,
			c.id, nodeID, c.round, string(TaskPending)).Scan(&remaining); err != nil {
			return err
		}
		if remaining > 0 {
			_, err := tx.ExecContext(ctx, `UPDATE cases SET updated_at = ? WHERE id = ?`, ts, c.id)
			return err
		}
	}

	// 节点通过。
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET status = ? WHERE case_id = ? AND node_id = ?`,
		string(NodeApproved), c.id, nodeID); err != nil {
		return err
	}

	// 阶段是否完成：当前阶段还有未完成节点则等待。
	var pendingNodes int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE case_id = ? AND stage = ? AND status = ?`,
		c.id, c.currentStage, string(NodePending)).Scan(&pendingNodes); err != nil {
		return err
	}
	if pendingNodes > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE cases SET updated_at = ? WHERE id = ?`, ts, c.id)
		return err
	}

	// 阶段完成：进入下一阶段，或全案通过。
	var totalStages int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT stage) FROM nodes WHERE case_id = ?`, c.id).Scan(&totalStages); err != nil {
		return err
	}
	if c.currentStage+1 >= totalStages {
		return setCaseStatus(ctx, tx, c.id, CaseApproved, c.currentStage, c.returnStage, c.round, ts)
	}
	next := c.currentStage + 1
	if err := setCaseStatus(ctx, tx, c.id, CaseRunning, next, c.returnStage, 0, ts); err != nil {
		return err
	}
	return spawnStageTasks(ctx, tx, c.id, next, 0, ts)
}

// resubmit 处理申请人重新提交。
func resubmit(ctx context.Context, tx *sql.Tx, reqID, caseID, actor string) error {
	c, err := loadCase(ctx, tx, caseID)
	if err != nil {
		return err
	}
	switch c.status {
	case CaseApproved, CaseRejected, CaseWithdrawn:
		return fmt.Errorf("%w: case %s is %s", ErrCaseClosed, caseID, c.status)
	case CaseRunning:
		return fmt.Errorf("%w: case %s is running, not returned", ErrCaseNotReturned, caseID)
	}
	if actor != c.applicant {
		return fmt.Errorf("%w: %s is not the applicant of case %s", ErrNotApplicant, actor, caseID)
	}

	ts := formatTime(now())
	newRound := c.round + 1
	// 退回阶段的节点进入新一轮；审批人名单沿用 node_assignees（含加签、转办结果）。
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET round = ? WHERE case_id = ? AND stage = ?`,
		newRound, caseID, c.returnStage); err != nil {
		return err
	}
	if err := setCaseStatus(ctx, tx, caseID, CaseRunning, c.returnStage, c.returnStage, newRound, ts); err != nil {
		return err
	}
	if err := spawnStageTasks(ctx, tx, caseID, c.returnStage, newRound, ts); err != nil {
		return err
	}
	return writeAudit(ctx, tx, caseID, reqID, ActionResubmit, actor, map[string]string{
		"stage": fmt.Sprint(c.returnStage),
		"round": fmt.Sprint(newRound),
	})
}

// addSign 给当前阶段未完成节点加一名审批人。
func addSign(ctx context.Context, tx *sql.Tx, reqID, caseID, nodeID, actor, assignee string) error {
	if assignee == "" || actor == "" {
		return fmt.Errorf("%w: actor/assignee is empty", ErrInvalidArgument)
	}
	c, err := loadCase(ctx, tx, caseID)
	if err != nil {
		return err
	}
	switch c.status {
	case CaseApproved, CaseRejected, CaseWithdrawn:
		return fmt.Errorf("%w: case %s is %s", ErrCaseClosed, caseID, c.status)
	case CasePendingRevision:
		return fmt.Errorf("%w: case %s is waiting for resubmission", ErrCaseNotRunning, caseID)
	}
	var nodeStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM nodes WHERE case_id = ? AND node_id = ? AND stage = ?`,
		caseID, nodeID, c.currentStage).Scan(&nodeStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node %s is not in the current stage of case %s", ErrNodeNotFound, nodeID, caseID)
	}
	if err != nil {
		return err
	}
	if nodeStatus != string(NodePending) {
		return fmt.Errorf("%w: node %s is already %s", ErrNodeNotFound, nodeID, nodeStatus)
	}
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_assignees WHERE case_id = ? AND node_id = ? AND assignee = ?`,
		caseID, nodeID, assignee).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return fmt.Errorf("%w: %s is already an approver of node %s", ErrAssigneeExists, assignee, nodeID)
	}

	ts := formatTime(now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_assignees(case_id, node_id, assignee) VALUES(?,?,?)`,
		caseID, nodeID, assignee); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(case_id, node_id, stage, round, assignee, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		caseID, nodeID, c.currentStage, c.round, assignee, string(TaskPending), ts, ts); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET updated_at = ? WHERE id = ?`, ts, caseID); err != nil {
		return err
	}
	return writeAudit(ctx, tx, caseID, reqID, ActionAddSign, actor, map[string]string{
		"node_id":  nodeID,
		"assignee": assignee,
	})
}

// transfer 把 from 在某节点的待办转给 to。
func transfer(ctx context.Context, tx *sql.Tx, reqID, caseID, nodeID, actor, from, to string) error {
	if actor == "" || from == "" || to == "" {
		return fmt.Errorf("%w: actor/from/to is empty", ErrInvalidArgument)
	}
	if from == to {
		return fmt.Errorf("%w: cannot transfer to the same person", ErrInvalidArgument)
	}
	c, err := loadCase(ctx, tx, caseID)
	if err != nil {
		return err
	}
	switch c.status {
	case CaseApproved, CaseRejected, CaseWithdrawn:
		return fmt.Errorf("%w: case %s is %s", ErrCaseClosed, caseID, c.status)
	case CasePendingRevision:
		return fmt.Errorf("%w: case %s is waiting for resubmission", ErrCaseNotRunning, caseID)
	}
	var nodeStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM nodes WHERE case_id = ? AND node_id = ? AND stage = ?`,
		caseID, nodeID, c.currentStage).Scan(&nodeStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: node %s is not in the current stage of case %s", ErrNodeNotFound, nodeID, caseID)
	}
	if err != nil {
		return err
	}
	if nodeStatus != string(NodePending) {
		return fmt.Errorf("%w: node %s is already %s", ErrNodeNotFound, nodeID, nodeStatus)
	}
	var toExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_assignees WHERE case_id = ? AND node_id = ? AND assignee = ?`,
		caseID, nodeID, to).Scan(&toExists); err != nil {
		return err
	}
	if toExists > 0 {
		return fmt.Errorf("%w: %s is already an approver of node %s", ErrAssigneeExists, to, nodeID)
	}

	// from 必须在当前轮持有该节点的有效待办。
	var taskID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM tasks WHERE case_id = ? AND node_id = ? AND round = ? AND assignee = ? AND status = ?`,
		caseID, nodeID, c.round, from, string(TaskPending)).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s has no pending task on node %s", ErrAssigneeNotInNode, from, nodeID)
	}
	if err != nil {
		return err
	}

	ts := formatTime(now())
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, result = ?, updated_at = ? WHERE id = ?`,
		string(TaskVoided), string(ActionTransfer), ts, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_assignees WHERE case_id = ? AND node_id = ? AND assignee = ?`,
		caseID, nodeID, from); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_assignees(case_id, node_id, assignee) VALUES(?,?,?)`,
		caseID, nodeID, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(case_id, node_id, stage, round, assignee, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		caseID, nodeID, c.currentStage, c.round, to, string(TaskPending), ts, ts); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cases SET updated_at = ? WHERE id = ?`, ts, caseID); err != nil {
		return err
	}
	return writeAudit(ctx, tx, caseID, reqID, ActionTransfer, actor, map[string]string{
		"node_id": nodeID,
		"from":    from,
		"to":      to,
	})
}

// withdraw 申请人撤回案件。
func withdraw(ctx context.Context, tx *sql.Tx, reqID, caseID, actor string) error {
	c, err := loadCase(ctx, tx, caseID)
	if err != nil {
		return err
	}
	switch c.status {
	case CaseApproved, CaseRejected, CaseWithdrawn:
		return fmt.Errorf("%w: case %s is %s", ErrCaseClosed, caseID, c.status)
	}
	if actor != c.applicant {
		return fmt.Errorf("%w: %s is not the applicant of case %s", ErrNotApplicant, actor, caseID)
	}
	ts := formatTime(now())
	if err := voidPendingTasks(ctx, tx, caseID, ts); err != nil {
		return err
	}
	if err := voidPendingNodes(ctx, tx, caseID); err != nil {
		return err
	}
	if err := setCaseStatus(ctx, tx, caseID, CaseWithdrawn, c.currentStage, c.returnStage, c.round, ts); err != nil {
		return err
	}
	return writeAudit(ctx, tx, caseID, reqID, ActionWithdraw, actor, nil)
}
