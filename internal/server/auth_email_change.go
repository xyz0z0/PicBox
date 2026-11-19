package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var errEmailExists = errors.New("邮箱已被其他账号使用")

func (a *App) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	if allowed, retryAfter := a.recoveryVerifyLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	email, err := normalizeEmail(payload.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "请输入有效的邮箱地址")
		return
	}

	record, err := a.loadUserByID(r.Context(), user.ID)
	switch {
	case errors.Is(err, errNoUser):
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改邮箱失败，请稍后重试")
		return
	}
	if record.disabled {
		writeError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用")
		return
	}
	if record.user.SessionVersion != user.SessionVersion {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "当前密码不正确，请重试")
		return
	}

	compareErr := a.comparePassword(record.passwordHash, payload.Password)
	if errors.Is(compareErr, errBcryptBusy) {
		writeRateLimited(w, passwordChangeRetryAfter)
		return
	}
	if compareErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "当前密码不正确，请重试")
		return
	}

	if email == record.user.Email {
		writeError(w, http.StatusBadRequest, "SAME_EMAIL", "新邮箱不能与当前邮箱相同")
		return
	}

	if allowed, retryAfter := a.recoveryMailLimiter.allow("email:" + email); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	taken, err := a.emailTaken(r.Context(), email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改邮箱失败，请稍后重试")
		return
	}
	if taken {
		writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已被其他账号使用")
		return
	}

	if !a.mailAvailable() {
		writeError(w, http.StatusServiceUnavailable, "MAIL_UNAVAILABLE", "邮件功能尚未配置，请联系管理员修改邮箱")
		return
	}
	if err := a.issueAccountToken(r.Context(), record.user.ID, record.user.Email, record.user.SessionVersion,
		email, accountTokenPurposeEmailChange); err != nil {
		writeError(w, http.StatusServiceUnavailable, "MAIL_UNAVAILABLE", "确认邮件发送失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleConfirmChangeEmail(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	if allowed, retryAfter := a.recoveryVerifyLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload struct {
		Token string `json:"token"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	email, err := a.confirmAccountEmail(r.Context(), user.ID, user.SessionVersion, payload.Token)
	switch {
	case errors.Is(err, errInvalidAccountToken):
		writeError(w, http.StatusBadRequest, "INVALID_TOKEN", "链接无效或已过期，请重新申请")
		return
	case errors.Is(err, errEmailExists):
		writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已被其他账号使用，请重新申请修改")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "修改邮箱失败，请稍后重试")
		return
	}

	// 邮箱变了：递增 sessionVersion 使旧会话失效，当前会话同样需要重新登录。
	a.clearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email})
}

func (a *App) emailTaken(ctx context.Context, email string) (bool, error) {
	var one int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM "User" WHERE email = ?`, email).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// confirmAccountEmail 消费 EMAIL_CHANGE 令牌并改邮箱。
//
// 令牌必须属于当前会话（userId + sessionVersion 都对得上）、未过期；成功后
// 清空该账号全部令牌并递增 sessionVersion（旧会话与旧链接都失效）。
// 新邮箱与已有账号冲突时返回 errEmailExists，事务整体回滚。
func (a *App) confirmAccountEmail(ctx context.Context, userID, sessionVersion int64, rawToken string) (string, error) {
	tokenHash, err := accountTokenHash(rawToken)
	if err != nil {
		return "", err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		tokenUserID  int64
		purpose      string
		tokenEmail   string
		currentEmail string
		tokenVersion int64
		expiresAt    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT userId, purpose, email, currentEmail, sessionVersion, expiresAt
		 FROM "AccountToken" WHERE tokenHash = ?`, tokenHash).
		Scan(&tokenUserID, &purpose, &tokenEmail, &currentEmail, &tokenVersion, &expiresAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", errInvalidAccountToken
	case err != nil:
		return "", err
	}

	now := coreNow()
	if purpose != accountTokenPurposeEmailChange ||
		tokenUserID != userID ||
		tokenVersion != sessionVersion ||
		expiresAt <= now {
		return "", errInvalidAccountToken
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM "AccountToken"
		 WHERE tokenHash = ? AND purpose = ? AND userId = ? AND expiresAt > ?`,
		tokenHash, accountTokenPurposeEmailChange, userID, now)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected != 1 {
		return "", errInvalidAccountToken
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE "User" SET email = ?, sessionVersion = sessionVersion + 1
		 WHERE id = ? AND email = ? AND sessionVersion = ? AND disabledAt IS NULL`,
		tokenEmail, userID, currentEmail, tokenVersion)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", errEmailExists
		}
		return "", err
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected != 1 {
		return "", errInvalidAccountToken
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE userId = ?`, userID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", errEmailExists
		}
		return "", fmt.Errorf("提交邮箱修改失败: %w", err)
	}
	return tokenEmail, nil
}
