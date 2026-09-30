package approval

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schema 是数据库结构定义，Open 时幂等执行。
const schema = `
CREATE TABLE IF NOT EXISTS cases (
    id            TEXT PRIMARY KEY,
    template_id   TEXT NOT NULL,
    template_name TEXT NOT NULL,
    template_json TEXT NOT NULL,
    applicant     TEXT NOT NULL,
    status        TEXT NOT NULL,
    current_stage INTEGER NOT NULL,
    return_stage  INTEGER NOT NULL DEFAULT -1,
    round         INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    case_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    stage   INTEGER NOT NULL,
    name    TEXT NOT NULL,
    mode    TEXT NOT NULL,
    status  TEXT NOT NULL,
    round   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (case_id, node_id)
);

CREATE TABLE IF NOT EXISTS node_assignees (
    case_id  TEXT NOT NULL,
    node_id  TEXT NOT NULL,
    assignee TEXT NOT NULL,
    PRIMARY KEY (case_id, node_id, assignee)
);

CREATE TABLE IF NOT EXISTS tasks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    case_id    TEXT NOT NULL,
    node_id    TEXT NOT NULL,
    stage      INTEGER NOT NULL,
    round      INTEGER NOT NULL,
    assignee   TEXT NOT NULL,
    status     TEXT NOT NULL,
    result     TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON tasks(assignee, status);
CREATE INDEX IF NOT EXISTS idx_tasks_case ON tasks(case_id, status);

CREATE TABLE IF NOT EXISTS requests (
    request_id  TEXT PRIMARY KEY,
    payload_hash TEXT NOT NULL,
    action      TEXT NOT NULL,
    case_id     TEXT NOT NULL,
    err_code    TEXT,
    err_msg     TEXT,
    result      TEXT,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    case_id    TEXT NOT NULL,
    request_id TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    payload    TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_case ON audit(case_id, seq);
`

// openDB 打开 SQLite 连接并设置 WAL、busy_timeout 等参数。
// _txlock=immediate 让每次事务以 BEGIN IMMEDIATE 开始，
// 多进程写同一案件（乃至同一库）时被串行化，效果等同逐个执行。
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接：进程内并发由连接池串行化，避免同一进程内两个事务互等锁。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return db, nil
}
