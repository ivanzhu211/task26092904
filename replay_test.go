package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// replayState 是审计重放用的内存模型，只依赖审计记录内容。
type replayState struct {
	status      CaseStatus
	stage       int
	returnStage int
	applicant   string
	nodeOrder   []string
	nodes       map[string]*replayNode
}

type replayNode struct {
	stage     int
	round     int
	mode      NodeMode
	status    NodeStatus
	assignees []string
	decided   []string
	pending   map[string]bool // 当前轮有效待办：assignee -> true
}

func newReplayState() *replayState {
	return &replayState{nodes: map[string]*replayNode{}, returnStage: -1}
}

func (s *replayState) spawn(stage int) {
	for _, id := range s.nodeOrder {
		n := s.nodes[id]
		if n.stage != stage {
			continue
		}
		n.pending = map[string]bool{}
		for _, a := range n.assignees {
			n.pending[a] = true
		}
	}
}

func (s *replayState) voidAllPending() {
	for _, n := range s.nodes {
		n.pending = map[string]bool{}
	}
}

func (s *replayState) apply(e AuditEntry) error {
	p := e.Payload
	switch e.Action {
	case ActionCreate:
		var tpl Template
		if err := json.Unmarshal([]byte(p["template_json"]), &tpl); err != nil {
			return err
		}
		s.status = CaseRunning
		s.stage = 0
		s.applicant = p["applicant"]
		for si, st := range tpl.Stages {
			for _, nt := range st.Nodes {
				s.nodes[nt.ID] = &replayNode{
					stage:     si,
					mode:      nt.Mode,
					status:    NodePending,
					assignees: append([]string{}, nt.Assignees...),
					pending:   map[string]bool{},
				}
				s.nodeOrder = append(s.nodeOrder, nt.ID)
			}
		}
		s.spawn(0)

	case ActionApprove:
		n := s.nodes[p["node_id"]]
		if n == nil || !n.pending[e.Actor] {
			return fmt.Errorf("replay approve: no pending task for %s on %s", e.Actor, p["node_id"])
		}
		delete(n.pending, e.Actor)
		n.decided = append(n.decided, e.Actor)
		if n.mode == ModeAny || len(n.pending) == 0 {
			n.pending = map[string]bool{}
			n.status = NodeApproved
			if s.stageComplete(s.stage) {
				if s.stage == s.maxStage() {
					s.status = CaseApproved
				} else {
					s.stage++
					s.spawn(s.stage)
				}
			}
		}

	case ActionReject:
		s.status = CaseRejected
		s.voidAllPending()
		for _, n := range s.nodes {
			if n.status == NodePending {
				n.status = NodeVoided
			}
		}

	case ActionReturn:
		s.status = CasePendingRevision
		s.returnStage = s.stage
		s.voidAllPending()

	case ActionResubmit:
		s.status = CaseRunning
		s.stage = s.returnStage
		for _, n := range s.nodes {
			if n.stage == s.returnStage {
				n.round++
				n.decided = nil
			}
		}
		s.spawn(s.returnStage)

	case ActionAddSign:
		n := s.nodes[p["node_id"]]
		n.assignees = append(n.assignees, p["assignee"])
		n.pending[p["assignee"]] = true

	case ActionTransfer:
		n := s.nodes[p["node_id"]]
		for i, a := range n.assignees {
			if a == p["from"] {
				n.assignees = append(n.assignees[:i], n.assignees[i+1:]...)
				break
			}
		}
		n.assignees = append(n.assignees, p["to"])
		delete(n.pending, p["from"])
		n.pending[p["to"]] = true

	case ActionWithdraw:
		s.status = CaseWithdrawn
		s.voidAllPending()
		for _, n := range s.nodes {
			if n.status == NodePending {
				n.status = NodeVoided
			}
		}
	}
	return nil
}

func (s *replayState) stageComplete(stage int) bool {
	for _, n := range s.nodes {
		if n.stage == stage && n.status != NodeApproved {
			return false
		}
	}
	return true
}

func (s *replayState) maxStage() int {
	max := 0
	for _, n := range s.nodes {
		if n.stage > max {
			max = n.stage
		}
	}
	return max
}

// verifyReplay 重放案件的全部审计记录，与引擎查询结果逐项比对；
// 同时校验审计里请求号不重复（没有任何操作生效两次）。
func verifyReplay(t *testing.T, e *Engine, caseID string) {
	t.Helper()
	ctx := context.Background()
	audit, err := e.ListAudit(ctx, caseID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	seenReq := map[string]bool{}
	state := newReplayState()
	for _, entry := range audit {
		if seenReq[entry.RequestID] {
			t.Fatalf("request %s took effect twice (seq %d)", entry.RequestID, entry.Seq)
		}
		seenReq[entry.RequestID] = true
		if err := state.apply(entry); err != nil {
			t.Fatalf("replay seq %d (%s): %v", entry.Seq, entry.Action, err)
		}
	}

	cv, nodes, err := e.GetCase(ctx, caseID)
	if err != nil {
		t.Fatalf("get case: %v", err)
	}
	if cv.Status != state.status {
		t.Fatalf("status mismatch: engine=%s replay=%s", cv.Status, state.status)
	}
	if cv.CurrentStage != state.stage {
		t.Fatalf("stage mismatch: engine=%d replay=%d", cv.CurrentStage, state.stage)
	}
	if cv.Applicant != state.applicant {
		t.Fatalf("applicant mismatch: engine=%s replay=%s", cv.Applicant, state.applicant)
	}

	engineNodes := map[string]NodeView{}
	for _, n := range nodes {
		engineNodes[n.NodeID] = n
	}
	if len(engineNodes) != len(state.nodes) {
		t.Fatalf("node count mismatch: engine=%d replay=%d", len(engineNodes), len(state.nodes))
	}
	for id, rn := range state.nodes {
		en, ok := engineNodes[id]
		if !ok {
			t.Fatalf("node %s missing in engine view", id)
		}
		if en.Status != rn.status {
			t.Fatalf("node %s status mismatch: engine=%s replay=%s", id, en.Status, rn.status)
		}
		if !equalSet(en.Assignees, rn.assignees) {
			t.Fatalf("node %s assignees mismatch: engine=%v replay=%v", id, en.Assignees, rn.assignees)
		}
		if !equalSet(en.DecidedBy, rn.decided) {
			t.Fatalf("node %s decided mismatch: engine=%v replay=%v", id, en.DecidedBy, rn.decided)
		}
	}

	// 待办比对：把重放模型里的有效待办与引擎里每个人名下的待办按
	// (case, node, assignee) 三元组比对。
	want := map[string]bool{}
	people := map[string]bool{}
	for id, rn := range state.nodes {
		for a := range rn.pending {
			want[fmt.Sprintf("%s|%s|%s", caseID, id, a)] = true
			people[a] = true
		}
	}
	got := map[string]bool{}
	for p := range people {
		todos, err := e.ListTodos(ctx, p)
		if err != nil {
			t.Fatalf("list todos for %s: %v", p, err)
		}
		for _, tv := range todos {
			if tv.CaseID == caseID {
				got[fmt.Sprintf("%s|%s|%s", tv.CaseID, tv.NodeID, tv.Assignee)] = true
			}
		}
	}
	if len(want) != len(got) {
		t.Fatalf("todo count mismatch: engine=%v replay=%v", keys(got), keys(want))
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("todo %s missing in engine", k)
		}
	}

	// 进行中的案件任何时候都至少有一个有效待办。
	if state.status == CaseRunning && len(got) == 0 {
		t.Fatal("running case has no pending todo")
	}
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
