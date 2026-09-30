package approval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// twoStageTemplate：阶段1 = 会签节点 n1(a,b) + 或签节点 n2(c,d)；阶段2 = 会签 n3(e)。
func twoStageTemplate() Template {
	return Template{
		ID:   "tpl1",
		Name: "请假审批",
		Stages: []StageTemplate{
			{Nodes: []NodeTemplate{
				{ID: "n1", Name: "部门会签", Mode: ModeAll, Assignees: []string{"a", "b"}},
				{ID: "n2", Name: "分管领导", Mode: ModeAny, Assignees: []string{"c", "d"}},
			}},
			{Nodes: []NodeTemplate{
				{ID: "n3", Name: "人事备案", Mode: ModeAll, Assignees: []string{"e"}},
			}},
		},
	}
}

func mustCreate(t *testing.T, e *Engine, reqID string, tpl Template, applicant string) string {
	t.Helper()
	id, err := e.CreateCase(context.Background(), reqID, tpl, applicant)
	if err != nil {
		t.Fatalf("create case: %v", err)
	}
	return id
}

// todoOf 取某人在案件上的唯一待办，拿不到就失败。
func todoOf(t *testing.T, e *Engine, assignee string) TaskView {
	t.Helper()
	todos, err := e.ListTodos(context.Background(), assignee)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	if len(todos) != 1 {
		t.Fatalf("expect 1 todo for %s, got %d", assignee, len(todos))
	}
	return todos[0]
}

func TestHappyPath(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create-1", twoStageTemplate(), "alice")

	cv, nodes, err := e.GetCase(ctx, caseID)
	if err != nil {
		t.Fatal(err)
	}
	if cv.Status != CaseRunning || cv.CurrentStage != 0 {
		t.Fatalf("unexpected case view: %+v", cv)
	}
	if len(nodes) != 3 {
		t.Fatalf("expect 3 nodes, got %d", len(nodes))
	}

	// 会签 n1：a 同意后不完成，b 同意后完成。
	if err := e.Approve(ctx, "req-a1", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	_, nodes, _ = e.GetCase(ctx, caseID)
	if nodes[0].Status != NodePending || len(nodes[0].DecidedBy) != 1 {
		t.Fatalf("n1 should still be pending: %+v", nodes[0])
	}
	if err := e.Approve(ctx, "req-b1", caseID, todoOf(t, e, "b").ID, "b"); err != nil {
		t.Fatal(err)
	}

	// 或签 n2：c 同意后 d 的待办作废。
	if err := e.Approve(ctx, "req-c1", caseID, todoOf(t, e, "c").ID, "c"); err != nil {
		t.Fatal(err)
	}
	if todos, _ := e.ListTodos(ctx, "d"); len(todos) != 0 {
		t.Fatalf("d's todo should be voided, got %d", len(todos))
	}

	// 进入阶段 2。
	cv, _, _ = e.GetCase(ctx, caseID)
	if cv.Status != CaseRunning || cv.CurrentStage != 1 {
		t.Fatalf("expect stage 1, got %+v", cv)
	}
	if err := e.Approve(ctx, "req-e1", caseID, todoOf(t, e, "e").ID, "e"); err != nil {
		t.Fatal(err)
	}
	cv, nodes, _ = e.GetCase(ctx, caseID)
	if cv.Status != CaseApproved {
		t.Fatalf("expect approved, got %s", cv.Status)
	}
	for _, n := range nodes {
		if n.Status != NodeApproved {
			t.Fatalf("node %s not approved: %s", n.NodeID, n.Status)
		}
	}

	// 审计：create + 4 次同意。
	audit, err := e.ListAudit(ctx, caseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 5 {
		t.Fatalf("expect 5 audit entries, got %d", len(audit))
	}
	for i, a := range audit {
		if a.Seq == 0 || a.RequestID == "" || a.Actor == "" || a.CreatedAt.IsZero() {
			t.Fatalf("audit entry %d incomplete: %+v", i, a)
		}
	}
}

func TestReject(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	if err := e.Reject(ctx, "req-rej", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	cv, _, _ := e.GetCase(ctx, caseID)
	if cv.Status != CaseRejected {
		t.Fatalf("expect rejected, got %s", cv.Status)
	}
	for _, u := range []string{"b", "c", "d"} {
		if todos, _ := e.ListTodos(ctx, u); len(todos) != 0 {
			t.Fatalf("%s should have no todo after reject", u)
		}
	}
	// 终态后的操作失败。
	if err := e.Approve(ctx, "req-late", caseID, 999, "b"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expect ErrTaskNotFound, got %v", err)
	}
}

func TestReturnAndResubmit(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	// a 先同意，随后 c 退回：a 的同意作废。
	if err := e.Approve(ctx, "req-a1", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Return(ctx, "req-ret", caseID, todoOf(t, e, "c").ID, "c"); err != nil {
		t.Fatal(err)
	}
	cv, _, _ := e.GetCase(ctx, caseID)
	if cv.Status != CasePendingRevision || cv.ReturnStage != 0 {
		t.Fatalf("expect pending_revision at stage 0, got %+v", cv)
	}
	if todos, _ := e.ListTodos(ctx, "b"); len(todos) != 0 {
		t.Fatal("b's todo should be voided after return")
	}

	// 非申请人不能重新提交。
	if err := e.Resubmit(ctx, "req-resub-bad", caseID, "bob"); !errors.Is(err, ErrNotApplicant) {
		t.Fatalf("expect ErrNotApplicant, got %v", err)
	}
	if err := e.Resubmit(ctx, "req-resub", caseID, "alice"); err != nil {
		t.Fatal(err)
	}
	cv, nodes, _ := e.GetCase(ctx, caseID)
	if cv.Status != CaseRunning || cv.CurrentStage != 0 {
		t.Fatalf("expect running at stage 0, got %+v", cv)
	}
	// 上一轮 a 的同意不算数。
	if len(nodes[0].DecidedBy) != 0 {
		t.Fatalf("decided_by should be reset, got %v", nodes[0].DecidedBy)
	}
	// 所有人重新有待办。
	for _, u := range []string{"a", "b", "c", "d"} {
		if todos, _ := e.ListTodos(ctx, u); len(todos) != 1 {
			t.Fatalf("%s should have 1 todo after resubmit, got %d", u, len(todos))
		}
	}
}

func TestAddSignAndTransfer(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	// 加签：会签节点 n1 加 f，三人都要同意。
	if err := e.AddSign(ctx, "req-add", caseID, "n1", "manager", "f"); err != nil {
		t.Fatal(err)
	}
	// 重复加签同一人失败。
	if err := e.AddSign(ctx, "req-add-dup", caseID, "n1", "manager", "f"); !errors.Is(err, ErrAssigneeExists) {
		t.Fatalf("expect ErrAssigneeExists, got %v", err)
	}
	if err := e.Approve(ctx, "req-a1", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(ctx, "req-b1", caseID, todoOf(t, e, "b").ID, "b"); err != nil {
		t.Fatal(err)
	}
	_, nodes, _ := e.GetCase(ctx, caseID)
	if nodes[0].Status != NodePending {
		t.Fatal("n1 should wait for addsigned f")
	}
	if err := e.Approve(ctx, "req-f1", caseID, todoOf(t, e, "f").ID, "f"); err != nil {
		t.Fatal(err)
	}

	// 转办：或签节点 n2 把 c 的待办转给 g。
	if err := e.Transfer(ctx, "req-tr", caseID, "n2", "manager", "c", "g"); err != nil {
		t.Fatal(err)
	}
	// c 的待办已作废，c 再操作失败。
	if todos, _ := e.ListTodos(ctx, "c"); len(todos) != 0 {
		t.Fatal("c's todo should be voided after transfer")
	}
	// 转给已在节点里的人失败。
	if err := e.Transfer(ctx, "req-tr-dup", caseID, "n2", "manager", "d", "g"); !errors.Is(err, ErrAssigneeExists) {
		t.Fatalf("expect ErrAssigneeExists, got %v", err)
	}
	// g 同意，或签通过。
	if err := e.Approve(ctx, "req-g1", caseID, todoOf(t, e, "g").ID, "g"); err != nil {
		t.Fatal(err)
	}
	cv, nodes, _ := e.GetCase(ctx, caseID)
	if cv.CurrentStage != 1 {
		t.Fatalf("expect stage 1, got %+v", cv)
	}
	if nodes[1].Status != NodeApproved {
		t.Fatalf("n2 should be approved, got %s", nodes[1].Status)
	}
}

func TestWithdraw(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	// 非申请人不能撤回。
	if err := e.Withdraw(ctx, "req-wd-bad", caseID, "bob"); !errors.Is(err, ErrNotApplicant) {
		t.Fatalf("expect ErrNotApplicant, got %v", err)
	}
	if err := e.Withdraw(ctx, "req-wd", caseID, "alice"); err != nil {
		t.Fatal(err)
	}
	cv, _, _ := e.GetCase(ctx, caseID)
	if cv.Status != CaseWithdrawn {
		t.Fatalf("expect withdrawn, got %s", cv.Status)
	}
	// 撤回后任何操作都失败。
	if err := e.Withdraw(ctx, "req-wd2", caseID, "alice"); !errors.Is(err, ErrCaseClosed) {
		t.Fatalf("expect ErrCaseClosed, got %v", err)
	}
	if err := e.Resubmit(ctx, "req-resub", caseID, "alice"); !errors.Is(err, ErrCaseClosed) {
		t.Fatalf("expect ErrCaseClosed, got %v", err)
	}
}

func TestTaskOwnershipAndState(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	taskA := todoOf(t, e, "a")
	// 别人的待办不能操作。
	if err := e.Approve(ctx, "req-steal", caseID, taskA.ID, "mallory"); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("expect ErrTaskNotOwned, got %v", err)
	}
	// 已完成的待办不能重复操作。
	if err := e.Approve(ctx, "req-a1", caseID, taskA.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(ctx, "req-a2", caseID, taskA.ID, "a"); !errors.Is(err, ErrTaskNotPending) {
		t.Fatalf("expect ErrTaskNotPending, got %v", err)
	}
	// 不存在的待办。
	if err := e.Approve(ctx, "req-ghost", caseID, 123456, "a"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expect ErrTaskNotFound, got %v", err)
	}
}

func TestRequestIDIdempotency(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	// 同一请求号重复创建：返回同一个案件，不新建。
	again, err := e.CreateCase(ctx, "req-create", twoStageTemplate(), "alice")
	if err != nil || again != caseID {
		t.Fatalf("duplicate create should return same case id, got %s %v", again, err)
	}
	// 同一请求号内容不同：报错。
	if _, err := e.CreateCase(ctx, "req-create", twoStageTemplate(), "bob"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("expect ErrRequestConflict, got %v", err)
	}

	// 同一请求号重复同意：第一次生效，第二次返回相同结果（不重复生效）。
	taskA := todoOf(t, e, "a")
	if err := e.Approve(ctx, "req-a1", caseID, taskA.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(ctx, "req-a1", caseID, taskA.ID, "a"); err != nil {
		t.Fatalf("replay should return first result (success), got %v", err)
	}
	audit, _ := e.ListAudit(ctx, caseID)
	if len(audit) != 2 { // create + approve
		t.Fatalf("expect 2 audit entries, got %d", len(audit))
	}
	// 同一请求号不同内容：报错。
	if err := e.Approve(ctx, "req-a1", caseID, 99999, "a"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("expect ErrRequestConflict, got %v", err)
	}

	// 第一次失败（业务错误）的请求，重试返回同样的失败。
	taskB := todoOf(t, e, "b")
	if err := e.Approve(ctx, "req-fail", caseID, taskB.ID, "mallory"); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("expect ErrTaskNotOwned, got %v", err)
	}
	if err := e.Approve(ctx, "req-fail", caseID, taskB.ID, "mallory"); !errors.Is(err, ErrTaskNotOwned) {
		t.Fatalf("replay should return first failure, got %v", err)
	}
	if audit, _ := e.ListAudit(ctx, caseID); len(audit) != 2 {
		t.Fatalf("failed op must not write audit, got %d", len(audit))
	}
}
