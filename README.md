# approval — 审批流转引擎

一个嵌入式的 Go 审批流转库，供上层政务流程平台直接调用（无接口层）。
数据持久化在 SQLite，支持多进程共用同一个数据库文件。

## 功能

- **流程模板**：若干阶段按顺序组成，每阶段含一个或多个并行节点；
  阶段内节点全部通过才进入下一阶段，最后一个阶段通过则案件通过。
  模板在发起案件时整体快照固化，之后不变。
- **节点方式**：会签（`ModeAll`，全部审批人同意）/ 或签（`ModeAny`，
  一人同意即可，其余人待办随即作废）。
- **驳回**：任一节点有人驳回，案件即驳回，所有待办作废。
- **退回修改**：案件回到申请人，所有待办作废；重新提交后从退回所在阶段
  重新开始，该阶段此前的同意全部作废，审批人名单沿用退回时的
  （含加签、转办结果）。
- **加签**：给未完成的节点加一名审批人（会签多等一人，或签多一个可选项）。
- **转办**：把某人的待办转给另一人，原审批人之后在该节点的操作全部失败。
- **撤回**：进行中或待修改的案件可由申请人撤回，所有待办作废，案件进入终态。
- **请求号幂等**：每个操作带请求号；同号重复提交返回第一次的结果，
  同号不同内容报错。
- **审计**：每个生效的操作写一条只追加的审计记录（操作人、动作、时间、参数）。
- **查询**：案件状态、各节点进度、某人名下的待办。

## 快速开始

```go
e, err := approval.Open("workflow.db")
if err != nil { log.Fatal(err) }
defer e.Close()

tpl := approval.Template{
    ID:   "leave",
    Name: "请假审批",
    Stages: []approval.StageTemplate{
        {Nodes: []approval.NodeTemplate{
            {ID: "dept", Name: "部门会签", Mode: approval.ModeAll, Assignees: []string{"a", "b"}},
            {ID: "boss", Name: "分管领导", Mode: approval.ModeAny, Assignees: []string{"c", "d"}},
        }},
        {Nodes: []approval.NodeTemplate{
            {ID: "hr", Name: "人事备案", Mode: approval.ModeAll, Assignees: []string{"e"}},
        }},
    },
}

ctx := context.Background()
caseID, err := e.CreateCase(ctx, "req-0001", tpl, "alice")

// 审批人查自己的待办，然后操作：
todos, _ := e.ListTodos(ctx, "a")
err = e.Approve(ctx, "req-0002", caseID, todos[0].ID, "a")

// 其他操作：Reject / Return / Resubmit / AddSign / Transfer / Withdraw
// 查询：GetCase（案件+节点进度）/ ListTodos / ListAudit
```

所有变更操作都返回 `error`；失败原因用 `errors.Is` 判定，例如
`ErrTaskNotPending`（待办已作废/已完成）、`ErrTaskNotOwned`（不是自己的待办）、
`ErrCaseClosed`（案件已终结）、`ErrRequestConflict`（同请求号不同内容）等。
对已作废、已完成或不属于自己的待办操作会明确失败，不会静默忽略。

## 并发与一致性设计

- 每个变更操作都在**单条 `BEGIN IMMEDIATE` 事务**内完成
  （`_txlock=immediate`），SQLite 写锁把同一数据库上的所有变更串行化，
  因此多名审批人同时同意、加签/转办与最后一个同意同时到达、撤回与审批
  同时到达等场景，结果都等同于按某个先后顺序逐个执行：同一轮里每个
  节点只完成一次，案件只推进一次。
- 数据库开启 **WAL** 与 `busy_timeout`，多进程共用同一文件时写者排队、
  读者不阻塞；进程内用单连接池串行化。
- **请求号幂等**：`requests` 表以请求号为主键，与业务变更在同一事务内
  落库。重复请求（包括第一次还没返回就重试）直接返回第一次存储的结果，
  不重复生效；同号不同内容返回 `ErrRequestConflict`。
- 进行中的案件任何时候都至少有一条有效待办：阶段推进与下一阶段待办的
  生成在同一事务内完成。
- 进程重启后用 `Open` 重新打开同一文件即可恢复，案件与待办原样保留。

## 测试

```sh
go test ./...            # 全部测试
go test -race -count=3 ./...
```

覆盖内容：

- 业务规则：会签/或签、驳回、退回+重新提交、加签、转办、撤回、
  待办归属与状态校验、请求号幂等（`engine_basic_test.go`）。
- 并发场景（每种反复多轮）：多人同时同意（会签/或签）、加签/转办与
  最后一个同意同时到达、撤回与审批同时到达、同一请求号并发重试、
  200 个操作混合风暴（`concurrency_test.go`）。
- 跨进程：多进程同时审批同一案件、同一请求号跨进程并发、
  进程接力推进与重启恢复（`multiprocess_test.go`）。
- 审计重放校验：每轮结束后把审计记录从头按顺序重放
  （`replay_test.go`），得到的案件状态、节点进度与待办必须和引擎
  查询结果完全一致，且审计中不存在生效两次的请求号。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `types.go` | 模板/案件/节点/待办/审计类型与错误定义 |
| `store.go` | SQLite schema 与连接参数（WAL、busy_timeout、immediate 事务） |
| `tx.go` | 事务包装、请求号幂等、审计写入 |
| `engine.go` | 公开 API |
| `ops.go` | 各操作的事务内实现 |
| `query.go` | 查询实现 |
