package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	// 改密接口使用独立的有界限流桶，不共享注册/登录的配额。
	passwordChangeIPLimit    = 10
	passwordChangeUserLimit  = 5
	passwordChangeWindow     = 10 * time.Minute
	passwordChangeRetryAfter = time.Second
)

// errPasswordStale 表示读到的密码哈希或 sessionVersion 已被其他请求改掉。
// 条件更新命中 0 行时返回它，保证并发改密只有一个赢家。
var errPasswordStale = errors.New("密码或会话已变化")

// passwordChangePayload 用指针显式记录字段是否存在：缺字段与 JSON null 都必须
// 拒绝，不能退化成空字符串再交给密码规则判断。
type passwordChangePayload struct {
	CurrentPassword *string `json:"currentPassword"`
	NewPassword     *string `json:"newPassword"`
}

// handleChangePassword 实现登录后改密：POST /api/auth/password。
// 只接受登录 Cookie（不接受 API Key）；成功后递增 sessionVersion 并撤销该用户
// 全部 API Key，因此旧 Cookie 与旧 Key 全部失效，且不签发新会话。
func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := a.passwordLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload passwordChangePayload
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	// allowAPIKey=false：Bearer 请求在这里被拒绝，改密只能来自登录 Cookie。
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	if payload.CurrentPassword == nil || payload.NewPassword == nil {
		writeError(w, http.StatusBadRequest, "INVALID_PAYLOAD", "请提供当前密码和新密码")
		return
	}
	currentPassword := *payload.CurrentPassword
	newPassword := *payload.NewPassword

	// 新密码沿用注册/登录的同一套规则（6~72 字节）。
	if err := validatePassword(newPassword); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}
	// 超过 bcrypt 72 字节上限的输入永远不会匹配任何哈希，直接拒绝，不浪费一次比较。
	if currentPassword == "" || len(currentPassword) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "当前密码不正确")
		return
	}

	if allowed, retryAfter := a.passwordUserLimiter.allow("user:" + strconv.FormatInt(user.ID, 10)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	record, failure := a.loadPasswordChangeRecord(r, user)
	if failure != nil {
		writeError(w, failure.status, failure.code, failure.message)
		return
	}

	// bcrypt 比较、重新哈希都放在事务外：数据库只有单连接，不能在慢操作期间占着它。
	compareErr := a.comparePassword(record.passwordHash, currentPassword)
	if errors.Is(compareErr, errBcryptBusy) {
		writeRateLimited(w, passwordChangeRetryAfter)
		return
	}
	if compareErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "当前密码不正确")
		return
	}

	newHash, err := a.hashPassword(newPassword)
	if err != nil {
		if errors.Is(err, errBcryptBusy) {
			writeRateLimited(w, passwordChangeRetryAfter)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改密码失败，请稍后重试")
		return
	}

	switch err := a.applyPasswordChange(r, record, newHash); {
	case errors.Is(err, errPasswordStale):
		writeError(w, http.StatusConflict, "SESSION_STALE", "登录状态已变化，请重新登录后再修改密码")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改密码失败，请稍后重试")
		return
	}

	// 不签发新会话，只清除当前 Cookie；sessionVersion 递增让其他会话一并失效。
	a.clearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "密码已更新，请使用新密码重新登录",
	})
}

// loadPasswordChangeRecord 重新读取账号，确保拿到最新的 passwordHash/sessionVersion。
// 读到之前会话已被并发改密推进时返回 409，而不是拿旧状态去覆盖新密码。
func (a *App) loadPasswordChangeRecord(r *http.Request, user *User) (userRecord, *authFailure) {
	record, err := a.loadUserByID(r.Context(), user.ID)
	switch {
	case errors.Is(err, errNoUser):
		return userRecord{}, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期"}
	case err != nil:
		return userRecord{}, &authFailure{http.StatusInternalServerError, "INTERNAL_ERROR", "修改密码失败，请稍后重试"}
	}
	if record.disabled {
		return userRecord{}, &authFailure{http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用"}
	}
	if record.user.SessionVersion != user.SessionVersion {
		return userRecord{}, &authFailure{http.StatusConflict, "SESSION_STALE", "登录状态已变化，请重新登录后再修改密码"}
	}
	return record, nil
}

// applyPasswordChange 在单事务内完成：条件更新密码并递增 sessionVersion、
// 撤销该用户所有未撤销的 API Key、清除 mustChangePassword 标记。
// UPDATE 带上读到的 passwordHash 与 sessionVersion 作为条件，并发改密时只有一个
// 请求能命中（另一个得到 0 行 → errPasswordStale），不会让旧会话覆盖新密码。
func (a *App) applyPasswordChange(r *http.Request, record userRecord, newHash []byte) error {
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.ExecContext(ctx,
		`UPDATE "User"
		 SET passwordHash = ?, sessionVersion = sessionVersion + 1, mustChangePassword = 0
		 WHERE id = ? AND passwordHash = ? AND sessionVersion = ? AND disabledAt IS NULL`,
		string(newHash), record.user.ID, record.passwordHash, record.user.SessionVersion)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errPasswordStale
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE "ApiKey" SET revokedAt = ?
		 WHERE userId = ? AND revokedAt IS NULL`,
		now, record.user.ID); err != nil {
		return err
	}

	return tx.Commit()
}
