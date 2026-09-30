package approval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// TestWorkerHelper 是被重新执行的子进程入口；正常测试进程中直接返回。
func TestWorkerHelper(t *testing.T) {
	if os.Getenv("APPROVAL_TEST_WORKER") == "" {
		return
	}
	// 子进程模式：执行完直接退出，不走 testing 框架。
	code := workerMain(os.Args)
	// 用日志标记子进程结果，父进程读退出码。
	fmt.Fprintf(os.Stderr, "worker exit %d\n", code)
	os.Exit(code)
}

// workerMain 解析参数并对共享数据库执行一个操作。
// 用法（-- 之后）：
//
//	approve   <db> <caseID> <reqID> <assignee>   找到 assignee 在该案件的待办并同意
//	reject    <db> <caseID> <reqID> <assignee>   同上但驳回
//	withdraw  <db> <caseID> <reqID> <actor>      撤回
//	todos     <db> <assignee>                    打印待办数量
func workerMain(argv []string) int {
	args := argv
	for i, a := range argv {
		if a == "--" {
			args = argv[i+1:]
			break
		}
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "worker: no command")
		return 2
	}
	ctx := context.Background()
	cmd := args[0]

	switch cmd {
	case "approve", "reject":
		if len(args) != 5 {
			fmt.Fprintln(os.Stderr, "worker: bad args")
			return 2
		}
		db, caseID, reqID, assignee := args[1], args[2], args[3], args[4]
		e, err := Open(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker open:", err)
			return 2
		}
		defer e.Close()
		todos, err := e.ListTodos(ctx, assignee)
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker todos:", err)
			return 2
		}
		var taskID int64 = -1
		for _, tv := range todos {
			if tv.CaseID == caseID {
				taskID = tv.ID
			}
		}
		if taskID < 0 {
			fmt.Fprintln(os.Stderr, "worker: no todo for", assignee)
			return 3
		}
		if cmd == "approve" {
			err = e.Approve(ctx, reqID, caseID, taskID, assignee)
		} else {
			err = e.Reject(ctx, reqID, caseID, taskID, assignee)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker op:", err)
			return 3
		}
		return 0

	case "withdraw":
		if len(args) != 5 {
			return 2
		}
		e, err := Open(args[1])
		if err != nil {
			return 2
		}
		defer e.Close()
		if err := e.Withdraw(ctx, args[3], args[2], args[4]); err != nil {
			fmt.Fprintln(os.Stderr, "worker op:", err)
			return 3
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "worker: unknown command", cmd)
	return 2
}

// runWorker 启动一个子进程执行 worker 命令。
func runWorker(t *testing.T, args ...string) error {
	t.Helper()
	full := append([]string{"-test.run=TestWorkerHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], full...)
	cmd.Env = append(os.Environ(), "APPROVAL_TEST_WORKER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("worker %v: %w\n%s", args, err, out)
	}
	return nil
}

// TestMultiProcessConcurrentApprove 多个进程同时对同一案件审批。
func TestMultiProcessConcurrentApprove(t *testing.T) {
	const rounds = 5
	const n = 8
	e := newEngine(t)
	ctx := context.Background()
	dbPath := e.dbPathForTest(t)
	assignees := names("p", n)

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-mp-c-%d", r), allSignTemplate(assignees...), "alice")
		errs := runConcurrent(n, func(i int) error {
			return runWorker(t, "approve", dbPath, caseID, fmt.Sprintf("req-mp-a-%d-%d", r, i), assignees[i])
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d worker %d: %v", r, i, err)
			}
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

// TestMultiProcessDuplicateRequest 同一请求号从不同进程并发到达：只生效一次。
func TestMultiProcessDuplicateRequest(t *testing.T) {
	const rounds = 5
	e := newEngine(t)
	ctx := context.Background()
	dbPath := e.dbPathForTest(t)

	for r := 0; r < rounds; r++ {
		caseID := mustCreate(t, e, fmt.Sprintf("req-mpp-c-%d", r), allSignTemplate("a", "b"), "alice")
		reqID := fmt.Sprintf("req-mpp-dup-%d", r)
		// 两个进程用同一个请求号审批 a 的待办。
		errs := runConcurrent(2, func(i int) error {
			return runWorker(t, "approve", dbPath, caseID, reqID, "a")
		})
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: duplicate request must return first result: %v", r, err)
			}
		}
		audit, _ := e.ListAudit(ctx, caseID)
		count := 0
		for _, a := range audit {
			if a.RequestID == reqID {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("round %d: request took effect %d times across processes", r, count)
		}
		verifyReplay(t, e, caseID)
	}
}

// TestMultiProcessHandoff 案件在多个进程间接力推进，并模拟重启恢复。
func TestMultiProcessHandoff(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/handoff.db"
	ctx := context.Background()

	// 进程 1（当前进程）：建案，a 同意。
	e1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	caseID, err := e1.CreateCase(ctx, "req-ho-c", allSignTemplate("a", "b"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	todos, _ := e1.ListTodos(ctx, "a")
	if err := e1.Approve(ctx, "req-ho-a", caseID, todos[0].ID, "a"); err != nil {
		t.Fatal(err)
	}
	// 模拟进程退出。
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	// 进程 2（子进程）：b 同意，案件完结。
	if err := runWorker(t, "approve", dbPath, caseID, "req-ho-b", "b"); err != nil {
		t.Fatal(err)
	}

	// 进程 3（当前进程重新打开）：状态原样恢复。
	e2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	cv, nodes, err := e2.GetCase(ctx, caseID)
	if err != nil {
		t.Fatal(err)
	}
	if cv.Status != CaseApproved {
		t.Fatalf("expect approved after restart, got %s", cv.Status)
	}
	if len(nodes) != 1 || nodes[0].Status != NodeApproved {
		t.Fatalf("nodes not recovered: %+v", nodes)
	}
	if todos, _ := e2.ListTodos(ctx, "a"); len(todos) != 0 {
		t.Fatal("no pending todos should remain")
	}
	verifyReplay(t, e2, caseID)
}

// dbPathForTest 从临时目录里的引擎反查数据库路径（测试辅助）。
func (e *Engine) dbPathForTest(t *testing.T) string {
	t.Helper()
	var seq, name, file string
	row := e.db.QueryRow(`SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'`)
	if err := row.Scan(&seq, &name, &file); err != nil {
		t.Fatalf("query db path: %v", err)
	}
	if file == "" {
		t.Fatal("engine is not backed by a file")
	}
	return file
}
