package approval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// getCase 查询案件状态与各节点进度。
func getCase(ctx context.Context, db *sql.DB, caseID string) (CaseView, []NodeView, error) {
	var (
		v                    CaseView
		createdAt, updatedAt string
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, template_id, template_name, applicant, status, current_stage, return_stage, created_at, updated_at
		 FROM cases WHERE id = ?`, caseID).
		Scan(&v.ID, &v.TemplateID, &v.TemplateName, &v.Applicant, &v.Status, &v.CurrentStage, &v.ReturnStage, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CaseView{}, nil, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return CaseView{}, nil, err
	}
	if v.CreatedAt, err = parseTime(createdAt); err != nil {
		return CaseView{}, nil, err
	}
	if v.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return CaseView{}, nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT node_id, name, stage, mode, status, round FROM nodes WHERE case_id = ? ORDER BY stage, node_id`, caseID)
	if err != nil {
		return CaseView{}, nil, err
	}
	defer rows.Close()
	type nodeRow struct {
		view  NodeView
		round int
	}
	var nodes []nodeRow
	for rows.Next() {
		var nr nodeRow
		if err := rows.Scan(&nr.view.NodeID, &nr.view.NodeName, &nr.view.Stage, &nr.view.Mode, &nr.view.Status, &nr.round); err != nil {
			return CaseView{}, nil, err
		}
		nodes = append(nodes, nr)
	}
	if err := rows.Err(); err != nil {
		return CaseView{}, nil, err
	}

	views := make([]NodeView, 0, len(nodes))
	for _, nr := range nodes {
		assignees, err := listAssignees(ctx, db, caseID, nr.view.NodeID)
		if err != nil {
			return CaseView{}, nil, err
		}
		decided, err := listDecidedBy(ctx, db, caseID, nr.view.NodeID, nr.round)
		if err != nil {
			return CaseView{}, nil, err
		}
		nr.view.Assignees = assignees
		nr.view.DecidedBy = decided
		views = append(views, nr.view)
	}
	return v, views, nil
}

func listAssignees(ctx context.Context, db *sql.DB, caseID, nodeID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT assignee FROM node_assignees WHERE case_id = ? AND node_id = ? ORDER BY rowid`, caseID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listDecidedBy 列出节点当前轮已同意的人。
func listDecidedBy(ctx context.Context, db *sql.DB, caseID, nodeID string, round int) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT assignee FROM tasks WHERE case_id = ? AND node_id = ? AND round = ? AND result = ? ORDER BY id`,
		caseID, nodeID, round, string(ActionApprove))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listTodos 查询某人名下的有效待办。
func listTodos(ctx context.Context, db *sql.DB, assignee string) ([]TaskView, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, case_id, node_id, stage, round, assignee, status, created_at, updated_at
		 FROM tasks WHERE assignee = ? AND status = ? ORDER BY id`, assignee, string(TaskPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskView{}
	for rows.Next() {
		var t TaskView
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.CaseID, &t.NodeID, &t.Stage, &t.Round, &t.Assignee, &t.Status, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if t.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// listAudit 按生效顺序列出案件的全部审计记录。
func listAudit(ctx context.Context, db *sql.DB, caseID string) ([]AuditEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT seq, case_id, request_id, action, actor, payload, created_at
		 FROM audit WHERE case_id = ? ORDER BY seq`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		var payload, createdAt string
		if err := rows.Scan(&a.Seq, &a.CaseID, &a.RequestID, &a.Action, &a.Actor, &payload, &createdAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &a.Payload); err != nil {
			return nil, err
		}
		if a.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
