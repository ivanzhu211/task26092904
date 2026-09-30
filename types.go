package approval

import (
	"errors"
	"fmt"
	"time"
)

// NodeMode 决定一个节点如何算作通过。
type NodeMode string

const (
	ModeAll NodeMode = "all" // 会签：全部审批人同意
	ModeAny NodeMode = "any" // 或签：一人同意即可
)

// CaseStatus 是案件的生命周期状态。
type CaseStatus string

const (
	CaseRunning         CaseStatus = "running"          // 审批进行中
	CasePendingRevision CaseStatus = "pending_revision" // 已退回，等待申请人重新提交
	CaseApproved        CaseStatus = "approved"         // 已通过（终态）
	CaseRejected        CaseStatus = "rejected"         // 已驳回（终态）
	CaseWithdrawn       CaseStatus = "withdrawn"        // 已撤回（终态）
)

// TaskStatus 是一条待办的状态。
type TaskStatus string

const (
	TaskPending TaskStatus = "pending" // 有效待办
	TaskDone    TaskStatus = "done"    // 已处理（同意/驳回/退回）
	TaskVoided  TaskStatus = "voided"  // 已作废（或签被抢、转办、驳回、退回、撤回等）
)

// NodeStatus 是案件内节点实例的状态。
type NodeStatus string

const (
	NodePending  NodeStatus = "pending"  // 等待处理
	NodeApproved NodeStatus = "approved" // 该节点已通过
	NodeVoided   NodeStatus = "voided"   // 随案件终结/退回而作废
)

// NodeTemplate 描述模板中一个节点的静态定义。
type NodeTemplate struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Mode      NodeMode `json:"mode"`
	Assignees []string `json:"assignees"`
}

// StageTemplate 是一个阶段，由若干并行节点组成。
type StageTemplate struct {
	Nodes []NodeTemplate `json:"nodes"`
}

// Template 是流程模板，发起案件时整体快照固化。
type Template struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Stages []StageTemplate `json:"stages"`
}

// Validate 校验模板基本合法性。
func (t Template) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("%w: template id is empty", ErrInvalidArgument)
	}
	if len(t.Stages) == 0 {
		return fmt.Errorf("%w: template has no stages", ErrInvalidArgument)
	}
	if len(t.Stages) > MaxStages {
		return fmt.Errorf("%w: template exceeds %d stages", ErrInvalidArgument, MaxStages)
	}
	for si, st := range t.Stages {
		if len(st.Nodes) == 0 {
			return fmt.Errorf("%w: stage %d has no nodes", ErrInvalidArgument, si)
		}
		seen := map[string]bool{}
		for _, n := range st.Nodes {
			if n.ID == "" {
				return fmt.Errorf("%w: stage %d has node with empty id", ErrInvalidArgument, si)
			}
			if seen[n.ID] {
				return fmt.Errorf("%w: duplicate node id %q", ErrInvalidArgument, n.ID)
			}
			seen[n.ID] = true
			if n.Mode != ModeAll && n.Mode != ModeAny {
				return fmt.Errorf("%w: node %q has invalid mode %q", ErrInvalidArgument, n.ID, n.Mode)
			}
			if len(n.Assignees) == 0 {
				return fmt.Errorf("%w: node %q has no assignees", ErrInvalidArgument, n.ID)
			}
			dup := map[string]bool{}
			for _, a := range n.Assignees {
				if a == "" {
					return fmt.Errorf("%w: node %q has empty assignee", ErrInvalidArgument, n.ID)
				}
				if dup[a] {
					return fmt.Errorf("%w: node %q has duplicate assignee %q", ErrInvalidArgument, n.ID, a)
				}
				dup[a] = true
			}
		}
	}
	return nil
}

// MaxStages 是单个流程允许的最大阶段数。
const MaxStages = 25

// CaseView 是案件状态查询结果。
type CaseView struct {
	ID           string     `json:"id"`
	TemplateID   string     `json:"template_id"`
	TemplateName string     `json:"template_name"`
	Applicant    string     `json:"applicant"`
	Status       CaseStatus `json:"status"`
	CurrentStage int        `json:"current_stage"` // 0 起；终态时为最后操作所在阶段
	ReturnStage  int        `json:"return_stage"`  // 被退回时所在阶段，-1 表示未曾退回
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// NodeView 是案件内一个节点实例的进度。
type NodeView struct {
	NodeID    string     `json:"node_id"`
	NodeName  string     `json:"node_name"`
	Stage     int        `json:"stage"`
	Mode      NodeMode   `json:"mode"`
	Status    NodeStatus `json:"status"`
	Assignees []string   `json:"assignees"`  // 当前审批人名单（含加签、转办结果）
	DecidedBy []string   `json:"decided_by"` // 本轮已同意的人
}

// TaskView 是一条待办/已办记录。
type TaskView struct {
	ID        int64      `json:"id"`
	CaseID    string     `json:"case_id"`
	NodeID    string     `json:"node_id"`
	Stage     int        `json:"stage"`
	Round     int        `json:"round"` // 同一阶段被退回重来的轮次，0 起
	Assignee  string     `json:"assignee"`
	Status    TaskStatus `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Action 是审计记录里的动作类型。
type Action string

const (
	ActionCreate   Action = "create"
	ActionApprove  Action = "approve"
	ActionReject   Action = "reject"
	ActionReturn   Action = "return"
	ActionResubmit Action = "resubmit"
	ActionAddSign  Action = "addsign"
	ActionTransfer Action = "transfer"
	ActionWithdraw Action = "withdraw"
)

// AuditEntry 是一条只追加的审计记录。
type AuditEntry struct {
	Seq       int64             `json:"seq"`
	CaseID    string            `json:"case_id"`
	RequestID string            `json:"request_id"`
	Action    Action            `json:"action"`
	Actor     string            `json:"actor"`
	Payload   map[string]string `json:"payload"`
	CreatedAt time.Time         `json:"created_at"`
}

// 错误定义：调用方可通过 errors.Is 判定失败原因。
var (
	ErrInvalidArgument   = errors.New("invalid argument")
	ErrCaseNotFound      = errors.New("case not found")
	ErrTaskNotFound      = errors.New("task not found")
	ErrTaskNotPending    = errors.New("task is not pending (already done or voided)")
	ErrTaskNotOwned      = errors.New("task does not belong to actor")
	ErrCaseNotRunning    = errors.New("case is not in running status")
	ErrCaseNotReturned   = errors.New("case is not waiting for resubmission")
	ErrCaseClosed        = errors.New("case is closed (approved/rejected/withdrawn)")
	ErrNotApplicant      = errors.New("actor is not the applicant")
	ErrNodeNotFound      = errors.New("node not found in current stage")
	ErrAssigneeExists    = errors.New("target already participates in the node")
	ErrAssigneeNotInNode = errors.New("source is not a pending approver of the node")
	ErrRequestConflict   = errors.New("request id reused with different payload")
	ErrEmptyRequestID    = errors.New("request id is empty")
)
