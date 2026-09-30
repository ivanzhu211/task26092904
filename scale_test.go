package approval

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestWithdrawReturnedCase 待修改状态也可以撤回。
func TestWithdrawReturnedCase(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")

	if err := e.Return(ctx, "req-ret", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	cv, _, _ := e.GetCase(ctx, caseID)
	if cv.Status != CasePendingRevision {
		t.Fatalf("expect pending_revision, got %s", cv.Status)
	}
	if err := e.Withdraw(ctx, "req-wd", caseID, "alice"); err != nil {
		t.Fatal(err)
	}
	cv, _, _ = e.GetCase(ctx, caseID)
	if cv.Status != CaseWithdrawn {
		t.Fatalf("expect withdrawn, got %s", cv.Status)
	}
	if err := e.Resubmit(ctx, "req-resub", caseID, "alice"); !errors.Is(err, ErrCaseClosed) {
		t.Fatalf("expect ErrCaseClosed, got %v", err)
	}
	verifyReplay(t, e, caseID)
}

// TestManyCasesParallel 大量案件并行推进，互不干扰。
func TestManyCasesParallel(t *testing.T) {
	const cases = 100
	e := newEngine(t)
	ctx := context.Background()

	caseIDs := make([]string, cases)
	errs := runConcurrent(cases, func(i int) error {
		id, err := e.CreateCase(ctx, fmt.Sprintf("req-mc-c-%d", i), allSignTemplate("a", "b"), "alice")
		if err == nil {
			caseIDs[i] = id
		}
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// 每个案件：a 同意 -> b 同意 -> 通过，全部并发。
	errs = runConcurrent(cases, func(i int) error {
		todos, err := e.ListTodos(ctx, "a")
		if err != nil {
			return err
		}
		for _, tv := range todos {
			if tv.CaseID == caseIDs[i] {
				return e.Approve(ctx, fmt.Sprintf("req-mc-a-%d", i), caseIDs[i], tv.ID, "a")
			}
		}
		return fmt.Errorf("no todo for a on case %s", caseIDs[i])
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("approve a %d: %v", i, err)
		}
	}
	errs = runConcurrent(cases, func(i int) error {
		todos, err := e.ListTodos(ctx, "b")
		if err != nil {
			return err
		}
		for _, tv := range todos {
			if tv.CaseID == caseIDs[i] {
				return e.Approve(ctx, fmt.Sprintf("req-mc-b-%d", i), caseIDs[i], tv.ID, "b")
			}
		}
		return fmt.Errorf("no todo for b on case %s", caseIDs[i])
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("approve b %d: %v", i, err)
		}
	}
	for i, id := range caseIDs {
		cv, _, err := e.GetCase(ctx, id)
		if err != nil || cv.Status != CaseApproved {
			t.Fatalf("case %d: expect approved, got %s %v", i, cv.Status, err)
		}
	}
	// 抽查若干案件做重放校验。
	for i := 0; i < cases; i += 10 {
		verifyReplay(t, e, caseIDs[i])
	}
}
