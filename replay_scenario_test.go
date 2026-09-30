package approval

import (
	"context"
	"fmt"
	"testing"
)

// TestReplayMixedScenario 混合场景：加签、转办、退回、重新提交、或签抢单，
// 每个阶段结束后都做审计重放校验。
func TestReplayMixedScenario(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	caseID := mustCreate(t, e, "req-create", twoStageTemplate(), "alice")
	verifyReplay(t, e, caseID)

	// 加签 f 到会签节点 n1，转办 n2 的 c -> g。
	if err := e.AddSign(ctx, "req-add", caseID, "n1", "mgr", "f"); err != nil {
		t.Fatal(err)
	}
	if err := e.Transfer(ctx, "req-tr", caseID, "n2", "mgr", "c", "g"); err != nil {
		t.Fatal(err)
	}
	verifyReplay(t, e, caseID)

	// a 同意后，g 把案件退回。
	if err := e.Approve(ctx, "req-a1", caseID, todoOf(t, e, "a").ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Return(ctx, "req-ret", caseID, todoOf(t, e, "g").ID, "g"); err != nil {
		t.Fatal(err)
	}
	verifyReplay(t, e, caseID)

	// 重新提交：退回阶段重开，名单沿用（n1 含 f，n2 是 d 和 g）。
	if err := e.Resubmit(ctx, "req-resub", caseID, "alice"); err != nil {
		t.Fatal(err)
	}
	verifyReplay(t, e, caseID)
	_, nodes, _ := e.GetCase(ctx, caseID)
	if !equalSet(nodes[0].Assignees, []string{"a", "b", "f"}) {
		t.Fatalf("n1 assignees should keep addsign result, got %v", nodes[0].Assignees)
	}
	if !equalSet(nodes[1].Assignees, []string{"d", "g"}) {
		t.Fatalf("n2 assignees should keep transfer result, got %v", nodes[1].Assignees)
	}

	// 走完整个流程。
	step := 0
	for {
		cv, _, _ := e.GetCase(ctx, caseID)
		if cv.Status != CaseRunning {
			break
		}
		// 找出当前所有待办并逐个同意（或签节点只同意第一个）。
		progressed := false
		seenNode := map[string]bool{}
		for _, u := range []string{"a", "b", "f", "d", "g", "e"} {
			todos, _ := e.ListTodos(ctx, u)
			for _, tv := range todos {
				if tv.CaseID != caseID || seenNode[tv.NodeID] {
					continue
				}
				seenNode[tv.NodeID] = true
				if err := e.Approve(ctx, fmt.Sprintf("req-ap-%d-%s", step, u), caseID, tv.ID, u); err != nil {
					t.Fatal(err)
				}
				progressed = true
			}
		}
		if !progressed {
			t.Fatal("no progress made")
		}
		verifyReplay(t, e, caseID)
		step++
	}
	cv, _, _ := e.GetCase(ctx, caseID)
	if cv.Status != CaseApproved {
		t.Fatalf("expect approved, got %s", cv.Status)
	}
	verifyReplay(t, e, caseID)
}
