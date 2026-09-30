package approval

import (
	"context"
	"database/sql"
	"errors"
)

// Engine 是审批流转引擎。通过 Open 打开或创建一个 SQLite 数据库文件。
// 同一个数据库文件可以被多个进程的多个 Engine 实例同时使用；
// 所有变更都在单条 IMMEDIATE 事务内完成，效果等同于逐个串行执行。
type Engine struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库文件并初始化 schema。
func Open(path string) (*Engine, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	return &Engine{db: db}, nil
}

// Close 关闭引擎。
func (e *Engine) Close() error {
	return e.db.Close()
}

// CreateCase 按模板发起一个案件，模板内容随案件快照固化。
// 返回案件 ID。案件创建后进入第一阶段，生成第一批待办。
func (e *Engine) CreateCase(ctx context.Context, reqID string, tpl Template, applicant string) (string, error) {
	if err := tpl.Validate(); err != nil {
		return "", err
	}
	if applicant == "" {
		return "", errors.Join(ErrInvalidArgument, errors.New("applicant is empty"))
	}
	payload := map[string]any{"template": tpl, "applicant": applicant}
	return e.runOp(ctx, reqID, string(ActionCreate), "", payload, func(tx *sql.Tx) (string, error) {
		return createCase(ctx, tx, reqID, tpl, applicant)
	})
}

// Approve 审批人对自己的一条待办点同意。
func (e *Engine) Approve(ctx context.Context, reqID, caseID string, taskID int64, actor string) error {
	payload := map[string]any{"task_id": taskID, "actor": actor}
	_, err := e.runOp(ctx, reqID, string(ActionApprove), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", decide(ctx, tx, reqID, caseID, taskID, actor, ActionApprove)
	})
	return err
}

// Reject 审批人驳回，案件立即进入驳回终态，所有待办作废。
func (e *Engine) Reject(ctx context.Context, reqID, caseID string, taskID int64, actor string) error {
	payload := map[string]any{"task_id": taskID, "actor": actor}
	_, err := e.runOp(ctx, reqID, string(ActionReject), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", decide(ctx, tx, reqID, caseID, taskID, actor, ActionReject)
	})
	return err
}

// Return 审批人退回修改：案件回到申请人，所有待办作废。
func (e *Engine) Return(ctx context.Context, reqID, caseID string, taskID int64, actor string) error {
	payload := map[string]any{"task_id": taskID, "actor": actor}
	_, err := e.runOp(ctx, reqID, string(ActionReturn), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", decide(ctx, tx, reqID, caseID, taskID, actor, ActionReturn)
	})
	return err
}

// Resubmit 申请人重新提交：案件从退回所在阶段重新开始，
// 该阶段此前的同意全部作废，审批人名单沿用退回时的（含加签、转办结果）。
func (e *Engine) Resubmit(ctx context.Context, reqID, caseID, actor string) error {
	payload := map[string]any{"actor": actor}
	_, err := e.runOp(ctx, reqID, string(ActionResubmit), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", resubmit(ctx, tx, reqID, caseID, actor)
	})
	return err
}

// AddSign 给当前阶段某个未完成的节点增加一名审批人。
func (e *Engine) AddSign(ctx context.Context, reqID, caseID, nodeID, actor, assignee string) error {
	payload := map[string]any{"node_id": nodeID, "actor": actor, "assignee": assignee}
	_, err := e.runOp(ctx, reqID, string(ActionAddSign), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", addSign(ctx, tx, reqID, caseID, nodeID, actor, assignee)
	})
	return err
}

// Transfer 把 from 在某节点的待办转给 to，from 之后在该节点的操作全部失败。
func (e *Engine) Transfer(ctx context.Context, reqID, caseID, nodeID, actor, from, to string) error {
	payload := map[string]any{"node_id": nodeID, "actor": actor, "from": from, "to": to}
	_, err := e.runOp(ctx, reqID, string(ActionTransfer), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", transfer(ctx, tx, reqID, caseID, nodeID, actor, from, to)
	})
	return err
}

// Withdraw 申请人撤回进行中或待修改的案件，所有待办作废，案件进入终态。
func (e *Engine) Withdraw(ctx context.Context, reqID, caseID, actor string) error {
	payload := map[string]any{"actor": actor}
	_, err := e.runOp(ctx, reqID, string(ActionWithdraw), caseID, payload, func(tx *sql.Tx) (string, error) {
		return "", withdraw(ctx, tx, reqID, caseID, actor)
	})
	return err
}

// GetCase 查询案件状态与各节点进度。
func (e *Engine) GetCase(ctx context.Context, caseID string) (CaseView, []NodeView, error) {
	return getCase(ctx, e.db, caseID)
}

// ListTodos 查询某人名下的待办。
func (e *Engine) ListTodos(ctx context.Context, assignee string) ([]TaskView, error) {
	return listTodos(ctx, e.db, assignee)
}

// ListAudit 按生效顺序列出案件的全部审计记录。
func (e *Engine) ListAudit(ctx context.Context, caseID string) ([]AuditEntry, error) {
	return listAudit(ctx, e.db, caseID)
}
