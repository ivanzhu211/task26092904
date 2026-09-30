package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// allSignTemplate 生成单阶段单会签节点的模板。
func allSignTemplate(assignees ...string) Template {
	return Template{
		ID:   "tpl-all",
		Name: "会签流程",
		Stages: []StageTemplate{
			{Nodes: []NodeTemplate{
				{ID: "n1", Name: "会签", Mode: ModeAll, Assignees: assignees},
			}},
		},
	}
}

func anySignTemplate(assignees ...string) Template {
	return Template{
		ID:   "tpl-any",
		Name: "或签流程",
		Stages: []StageTemplate{
			{Nodes: []NodeTemplate{
				{ID: "n1", Name: "或签", Mode: ModeAny, Assignees: assignees},
			}},
		},
	}
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// runConcurrent 同时启动 fn(i)（i 从 0 到 n-1），等待全部结束，返回各自错误。
func runConcurrent(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func countErrs(errs []error, want error) (matched, succeeded int) {
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, want) {
			matched++
		}
	}
	return
}

// TestConcurrentAllSign 多名审批人同时同意会签节点：全部生效，案件只推进一次。
func TestConcurrentAllSign(t *testing.T) {
	const rounds = 20
	const n = 20
	e := newEngine(t)
	ctx := context.Background()
	assignees := names("u", n)

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate(assignees...), "alice")
		todos := map[string]int64{}
		for _, u := range assignees {
			list, err := e.ListTodos(ctx, u)
			if err != nil || len(list) != 1 {
				t.Fatalf("round %d: %s should have 1 todo: %v %d", r, u, err, len(list))
			}
			todos[u] = list[0].ID
		}
		errs := runConcurrent(n, func(i int) error {
			u := assignees[i]
			return e.Approve(ctx, fmt.Sprintf("req-a-%d-%s", r, u), caseID, todos[u], u)
		})
		if _, ok := countErrs(errs, nil); ok != n {
			t.Fatalf("round %d: all approves should succeed, errs=%v", r, errs)
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseApproved {
			t.Fatalf("round %d: expect approved, got %s", r, cv.Status)
		}
		audit, _ := e.ListAudit(ctx, caseID)
		if len(audit) != n+1 {
			t.Fatalf("round %d: expect %d audit entries, got %d", r, n+1, len(audit))
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentAnySign 或签节点多人同时同意：只有一人生效，其余明确失败。
func TestConcurrentAnySign(t *testing.T) {
	const rounds = 20
	const n = 10
	e := newEngine(t)
	ctx := context.Background()
	assignees := names("u", n)

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), anySignTemplate(assignees...), "alice")
		todos := map[string]int64{}
		for _, u := range assignees {
			list, _ := e.ListTodos(ctx, u)
			todos[u] = list[0].ID
		}
		errs := runConcurrent(n, func(i int) error {
			u := assignees[i]
			return e.Approve(ctx, fmt.Sprintf("req-a-%d-%s", r, u), caseID, todos[u], u)
		})
		succeeded := 0
		for _, err := range errs {
			if err == nil {
				succeeded++
			} else if !errors.Is(err, ErrTaskNotPending) {
				t.Fatalf("round %d: unexpected error %v", r, err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("round %d: exactly one approve should win, got %d", r, succeeded)
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseApproved {
			t.Fatalf("round %d: expect approved, got %s", r, cv.Status)
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentLastApproveAndAddSign 加签与最后一个同意同时到达。
func TestConcurrentLastApproveAndAddSign(t *testing.T) {
	const rounds = 30
	e := newEngine(t)
	ctx := context.Background()

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate("a", "b"), "alice")
		if err := e.Approve(ctx, fmt.Sprintf("req-a-%d", r), caseID, todoOf(t, e, "a").ID, "a"); err != nil {
			t.Fatal(err)
		}
		bTask := todoOf(t, e, "b").ID
		errs := runConcurrent(2, func(i int) error {
			if i == 0 {
				return e.Approve(ctx, fmt.Sprintf("req-b-%d", r), caseID, bTask, "b")
			}
			return e.AddSign(ctx, fmt.Sprintf("req-add-%d", r), caseID, "n1", "mgr", "c")
		})
		approveErr, addErr := errs[0], errs[1]
		if approveErr != nil {
			t.Fatalf("round %d: b's approve must succeed, got %v", r, approveErr)
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if addErr == nil {
			// 加签先到：案件仍在进行，c 同意后通过。
			if cv.Status != CaseRunning {
				t.Fatalf("round %d: addsign won, case should be running, got %s", r, cv.Status)
			}
			if err := e.Approve(ctx, fmt.Sprintf("req-c2-%d", r), caseID, todoOf(t, e, "c").ID, "c"); err != nil {
				t.Fatal(err)
			}
			cv, _, _ = e.GetCase(ctx, caseID)
			if cv.Status != CaseApproved {
				t.Fatalf("round %d: expect approved after c, got %s", r, cv.Status)
			}
		} else {
			// 同意先到：节点已完成，加签失败。
			if !errors.Is(addErr, ErrNodeNotFound) && !errors.Is(addErr, ErrCaseClosed) {
				t.Fatalf("round %d: expect ErrNodeNotFound/ErrCaseClosed, got %v", r, addErr)
			}
			if cv.Status != CaseApproved {
				t.Fatalf("round %d: expect approved, got %s", r, cv.Status)
			}
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentLastApproveAndTransfer 转办与审批同时到达。
func TestConcurrentLastApproveAndTransfer(t *testing.T) {
	const rounds = 30
	e := newEngine(t)
	ctx := context.Background()

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate("a", "b"), "alice")
		aTask := todoOf(t, e, "a").ID
		bTask := todoOf(t, e, "b").ID
		errs := runConcurrent(2, func(i int) error {
			if i == 0 {
				return e.Transfer(ctx, fmt.Sprintf("req-tr-%d", r), caseID, "n1", "mgr", "a", "x")
			}
			return e.Approve(ctx, fmt.Sprintf("req-b-%d", r), caseID, bTask, "b")
		})
		trErr, bErr := errs[0], errs[1]
		if trErr != nil {
			t.Fatalf("round %d: transfer should succeed, got %v", r, trErr)
		}
		if bErr != nil {
			t.Fatalf("round %d: b's approve should succeed, got %v", r, bErr)
		}
		// a 的待办已被转走，a 再操作必须失败。
		if err := e.Approve(ctx, fmt.Sprintf("req-a-%d", r), caseID, aTask, "a"); !errors.Is(err, ErrTaskNotPending) {
			t.Fatalf("round %d: a's approve on transferred task must fail, got %v", r, err)
		}
		// x 接手后同意，案件通过。
		if err := e.Approve(ctx, fmt.Sprintf("req-x-%d", r), caseID, todoOf(t, e, "x").ID, "x"); err != nil {
			t.Fatal(err)
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseApproved {
			t.Fatalf("round %d: expect approved, got %s", r, cv.Status)
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentWithdrawAndApprove 撤回与审批同时到达。
func TestConcurrentWithdrawAndApprove(t *testing.T) {
	const rounds = 30
	e := newEngine(t)
	ctx := context.Background()

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate("a", "b"), "alice")
		aTask := todoOf(t, e, "a").ID
		errs := runConcurrent(2, func(i int) error {
			if i == 0 {
				return e.Withdraw(ctx, fmt.Sprintf("req-wd-%d", r), caseID, "alice")
			}
			return e.Approve(ctx, fmt.Sprintf("req-a-%d", r), caseID, aTask, "a")
		})
		wdErr, apErr := errs[0], errs[1]
		if wdErr != nil {
			t.Fatalf("round %d: withdraw should succeed, got %v", r, wdErr)
		}
		// 审批要么先于撤回到达（成功），要么待办已作废（明确失败）。
		if apErr != nil && !errors.Is(apErr, ErrTaskNotPending) {
			t.Fatalf("round %d: unexpected approve error %v", r, apErr)
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseWithdrawn {
			t.Fatalf("round %d: expect withdrawn, got %s", r, cv.Status)
		}
		// 此后任何操作都失败。
		if err := e.Approve(ctx, fmt.Sprintf("req-late-%d", r), caseID, aTask, "a"); err == nil {
			t.Fatalf("round %d: op on withdrawn case must fail", r)
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentDuplicateRequest 同一请求号在第一次未返回时并发到达：只生效一次。
func TestConcurrentDuplicateRequest(t *testing.T) {
	const rounds = 20
	const dup = 10
	e := newEngine(t)
	ctx := context.Background()

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate("a", "b"), "alice")
		aTask := todoOf(t, e, "a").ID
		reqID := fmt.Sprintf("req-dup-%d", r)
		errs := runConcurrent(dup, func(i int) error {
			return e.Approve(ctx, reqID, caseID, aTask, "a")
		})
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: duplicate request should return first result (success), got %v", r, err)
			}
		}
		// 审计里该请求号只出现一次。
		audit, _ := e.ListAudit(ctx, caseID)
		count := 0
		for _, a := range audit {
			if a.RequestID == reqID {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("round %d: request took effect %d times", r, count)
		}
		verifyReplay(t, e, caseID)
	}
}

// TestConcurrentStorm 200 个操作同时打到同一案件：同意、重复请求、加签、非法操作混合。
func TestConcurrentStorm(t *testing.T) {
	const rounds = 5
	const approvers = 100
	e := newEngine(t)
	ctx := context.Background()
	assignees := names("u", approvers)

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-c-%d", r), allSignTemplate(assignees...), "alice")
		todos := map[string]int64{}
		for _, u := range assignees {
			list, _ := e.ListTodos(ctx, u)
			todos[u] = list[0].ID
		}
		// 100 个同意 + 50 个重复请求 + 20 个加签（10 人各两次）+ 30 个非法操作 = 200。
		errs := runConcurrent(200, func(i int) error {
			switch {
			case i < 100:
				u := assignees[i]
				return e.Approve(ctx, fmt.Sprintf("req-a-%d-%s", r, u), caseID, todos[u], u)
			case i < 150:
				u := assignees[i-100]
				return e.Approve(ctx, fmt.Sprintf("req-a-%d-%s", r, u), caseID, todos[u], u) // 重复请求
			case i < 170:
				k := (i - 150) / 2
				return e.AddSign(ctx, fmt.Sprintf("req-add-%d-%d", r, k), caseID, "n1", "mgr", fmt.Sprintf("extra%d", k))
			default:
				return e.Approve(ctx, fmt.Sprintf("req-bad-%d-%d", r, i), caseID, todos["u0"], "mallory")
			}
		})
		for i, err := range errs {
			switch {
			case i < 170:
				if err != nil {
					t.Fatalf("round %d op %d: unexpected error %v", r, i, err)
				}
			default:
				if !errors.Is(err, ErrTaskNotOwned) && !errors.Is(err, ErrTaskNotPending) {
					t.Fatalf("round %d op %d: expect ownership/state error, got %v", r, i, err)
				}
			}
		}
		// 加签进来的人也要同意，案件才能完结。
		for k := 0; k < 10; k++ {
			u := fmt.Sprintf("extra%d", k)
			list, _ := e.ListTodos(ctx, u)
			for _, tv := range list {
				if tv.CaseID != caseID {
					continue
				}
				if err := e.Approve(ctx, fmt.Sprintf("req-extra-%d-%d", r, k), caseID, tv.ID, u); err != nil {
					t.Fatalf("round %d: extra approver %s: %v", r, u, err)
				}
			}
		}
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseApproved {
			t.Fatalf("round %d: expect approved, got %s", r, cv.Status)
		}
		// 审计：create + 100 个原始同意各出现一次；重复与非法操作不产生审计。
		audit, _ := e.ListAudit(ctx, caseID)
		count := map[string]int{}
		for _, a := range audit {
			count[a.RequestID]++
		}
		for _, u := range assignees {
			rid := fmt.Sprintf("req-a-%d-%s", r, u)
			if count[rid] != 1 {
				t.Fatalf("round %d: request %s took effect %d times", r, rid, count[rid])
			}
		}
		verifyReplay(t, e, caseID)
	}
}
