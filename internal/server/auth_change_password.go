package server

import (
	"errors"
	"net/http"
	"strconv"
)

// passwordChangeNewPayload 用指针显式记录字段是否存在：缺字段与 JSON null 都按
// “未提供”处理，返回与原版一致的 INVALID_PASSWORD，而不是 INVALID_BODY。
type passwordChangeNewPayload struct {
	OldPassword *string `json:"oldPassword"`
	NewPassword *string `json:"newPassword"`
}

// handleChangePasswordNew 实现原版 UI 使用的 POST /api/auth/change-password。
//
// 与原版 lib 契约一致（app/api/auth/change-password/route.ts）：
//   - 体 {oldPassword,newPassword}，只接受登录 Cookie（不接受 API Key）；
//   - oldPassword 为空 → 400 INVALID_PASSWORD「请输入原密码」；
//   - 新密码 <6 → 400 INVALID_PASSWORD；与原密码相同 → 400 SAME_PASSWORD；
//   - 旧密码不匹配 → 400 INVALID_CREDENTIALS；
//   - 成功：清除 mustChangePassword、sessionVersion+1，并用新版本重签 Cookie，
//     保持当前会话登录（其他设备旧 JWT 失效），但**不撤销 API Key**。
//
// 旧路径 /api/auth/password 保留原语义（清 Cookie + 撤销全部 API Key），
// 供 Go 预览页使用，避免与本路径的语义互相污染。
func (a *App) handleChangePasswordNew(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := a.passwordLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload passwordChangeNewPayload
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	if payload.OldPassword == nil || *payload.OldPassword == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", "请输入原密码")
		return
	}
	if payload.NewPassword == nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", "新密码至少 6 位")
		return
	}
	oldPassword := *payload.OldPassword
	newPassword := *payload.NewPassword
	if err := validatePassword(newPassword); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
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

	// 超过 bcrypt 72 字节上限的输入永远不会匹配任何哈希，直接拒绝，不浪费一次比较。
	if len(oldPassword) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "原密码不正确")
		return
	}

	// bcrypt 比较、重新哈希都放在事务外：数据库只有单连接，不能在慢操作期间占着它。
	compareErr := a.comparePassword(record.passwordHash, oldPassword)
	if errors.Is(compareErr, errBcryptBusy) {
		writeRateLimited(w, passwordChangeRetryAfter)
		return
	}
	if compareErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "原密码不正确")
		return
	}
	// 强制改密就是要换掉管理员生成的临时密码，原样填回等于没改。
	if newPassword == oldPassword {
		writeError(w, http.StatusBadRequest, "SAME_PASSWORD", "新密码不能与原密码相同")
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

	nextVersion, err := a.applyPasswordChangeKeepSession(r, record, newHash)
	switch {
	case errors.Is(err, errPasswordStale):
		writeError(w, http.StatusConflict, "SESSION_STALE", "登录状态已变化，请重新登录后再修改密码")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改密码失败，请稍后重试")
		return
	}

	token, err := a.signToken(record.user.ID, nextVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改密码失败，请稍后重试")
		return
	}
	a.setAuthCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// applyPasswordChangeKeepSession 在单事务内改密并递增 sessionVersion，
// 但保留 API Key。UPDATE 带上读到的 passwordHash 与 sessionVersion 作为条件，
// 并发改密只有一个赢家（另一个得到 0 行 → errPasswordStale），不会用旧会话覆盖新密码。
// 返回递增后的 sessionVersion，供调用方重签 Cookie。
func (a *App) applyPasswordChangeKeepSession(r *http.Request, record userRecord, newHash []byte) (int64, error) {
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`UPDATE "User"
		 SET passwordHash = ?, sessionVersion = sessionVersion + 1, mustChangePassword = 0
		 WHERE id = ? AND passwordHash = ? AND sessionVersion = ? AND disabledAt IS NULL`,
		string(newHash), record.user.ID, record.passwordHash, record.user.SessionVersion)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected != 1 {
		return 0, errPasswordStale
	}

	var version int64
	if err := tx.QueryRowContext(ctx,
		`SELECT sessionVersion FROM "User" WHERE id = ?`, record.user.ID).Scan(&version); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}
