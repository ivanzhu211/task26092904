package approval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// now 返回当前 UTC 时间，序列化到毫秒，保证入库格式稳定。
func now() time.Time {
	return time.Now().UTC()
}

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// storedError 用于把第一次执行的错误结果原样返回给重复请求。
type storedError struct {
	sentinel error
	msg      string
}

func (e *storedError) Error() string { return e.msg }
func (e *storedError) Unwrap() error { return e.sentinel }

// businessErrs 是所有可预期业务失败；只有它们会随请求号一起落库。
var businessErrs = []error{
	ErrInvalidArgument,
	ErrCaseNotFound,
	ErrTaskNotFound,
	ErrTaskNotPending,
	ErrTaskNotOwned,
	ErrCaseNotRunning,
	ErrCaseNotReturned,
	ErrCaseClosed,
	ErrNotApplicant,
	ErrNodeNotFound,
	ErrAssigneeExists,
	ErrAssigneeNotInNode,
	ErrRequestConflict,
}

// errCode 把业务错误映射为稳定编码；非业务错误返回空串。
func errCode(err error) (string, bool) {
	for _, s := range businessErrs {
		if errors.Is(err, s) {
			return s.Error(), true
		}
	}
	return "", false
}

func errFromCode(code, msg string) error {
	for _, s := range businessErrs {
		if s.Error() == code {
			return &storedError{sentinel: s, msg: msg}
		}
	}
	return errors.New(msg)
}

// payloadHash 计算请求内容的稳定摘要，用于识别同号不同内容。
func payloadHash(action, caseID string, payload any) string {
	b, _ := json.Marshal(payload)
	h := sha256.Sum256([]byte(action + "|" + caseID + "|" + string(b)))
	return hex.EncodeToString(h[:])
}

// opFunc 是单事务内执行的操作体；返回的 error 若是业务错误会随请求号落库。
type opFunc func(tx *sql.Tx) (result string, err error)

// runOp 在一条 IMMEDIATE 事务里执行操作，并处理请求号幂等：
//   - 请求号已存在且内容一致：直接返回第一次的结果，不产生任何效果；
//   - 请求号已存在但内容不同：返回 ErrRequestConflict；
//   - 否则执行操作，把结果（含业务失败）与请求号一起落库。
func (e *Engine) runOp(ctx context.Context, reqID, action, caseID string, payload any, fn opFunc) (string, error) {
	if reqID == "" {
		return "", ErrEmptyRequestID
	}
	hash := payloadHash(action, caseID, payload)

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	var storedHash, errCodeStr, errMsg, storedResult sql.NullString
	// result 列允许 NULL，用空串代表无返回值的请求。
	row := tx.QueryRowContext(ctx,
		`SELECT payload_hash, COALESCE(err_code,''), COALESCE(err_msg,''), COALESCE(result,'') FROM requests WHERE request_id = ?`, reqID)
	scanErr := row.Scan(&storedHash, &errCodeStr, &errMsg, &storedResult)
	switch {
	case scanErr == nil:
		if storedHash.String != hash {
			return "", fmt.Errorf("%w: request %q was already used for a different operation", ErrRequestConflict, reqID)
		}
		if errCodeStr.String != "" {
			return storedResult.String, errFromCode(errCodeStr.String, errMsg.String)
		}
		return storedResult.String, nil
	case !errors.Is(scanErr, sql.ErrNoRows):
		return "", scanErr
	}

	result, opErr := fn(tx)

	code, isBusiness := "", false
	errMsgStr := ""
	if opErr != nil {
		var biz bool
		code, biz = errCode(opErr)
		if !biz {
			// 非业务错误（数据库故障等）：整体回滚，不落请求记录，允许调用方重试。
			return "", opErr
		}
		isBusiness = true
		errMsgStr = opErr.Error()
	}
	_ = isBusiness // 保留语义标记：只有业务失败才走到这里落库

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO requests(request_id, payload_hash, action, case_id, err_code, err_msg, result, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		reqID, hash, action, caseID, nullStr(code), nullStr(errMsgStr), result, formatTime(now())); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	committed = true
	return result, opErr
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// writeAudit 在当前事务内追加一条审计记录。只有生效的操作才调用。
func writeAudit(ctx context.Context, tx *sql.Tx, caseID, reqID string, action Action, actor string, payload map[string]string) error {
	if payload == nil {
		payload = map[string]string{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit(case_id, request_id, action, actor, payload, created_at) VALUES(?,?,?,?,?,?)`,
		caseID, reqID, string(action), actor, string(b), formatTime(now()))
	return err
}
