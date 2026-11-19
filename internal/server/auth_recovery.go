package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	accountTokenPurposePasswordReset = "PASSWORD_RESET"
	accountTokenPurposeEmailChange   = "EMAIL_CHANGE"
	// 与原版一致：一次性链接 30 分钟内有效，重新申请后旧链接失效。
	accountTokenTTL = 30 * time.Minute
)

// 原版要求令牌是 64 位小写 hex（32 字节随机数）。格式不合法直接按无效处理，
// 不做任何数据库查询，避免拿任意字符串探测。
var accountTokenPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var (
	errInvalidAccountToken = errors.New("令牌无效或已过期")
	errSamePassword        = errors.New("新密码与原密码相同")
)

// accountTokenHash 校验令牌格式并返回入库用的哈希。
func accountTokenHash(raw string) (string, error) {
	if !accountTokenPattern.MatchString(raw) {
		return "", errInvalidAccountToken
	}
	return hashAccountTokenHex(raw), nil
}

// issueAccountToken 签发一次性令牌并投递邮件。
//
// 同一账号每种用途至多一条记录（UNIQUE(userId, purpose)）：重新申请会覆盖旧令牌，
// 旧链接因此立即失效。数据库只保存 sha256 哈希；投递失败时只删除本次写入的那条
// （按 tokenHash 条件删除），不能删掉并发重发产生的新令牌。
func (a *App) issueAccountToken(ctx context.Context, userID int64, currentEmail string, sessionVersion int64, targetEmail, purpose string) error {
	token, tokenHash, err := newAccountToken()
	if err != nil {
		return err
	}
	expiresAt := coreTimestamp(time.Now().Add(accountTokenTTL))
	now := coreNow()

	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO "AccountToken" (id, userId, purpose, tokenHash, email, currentEmail, sessionVersion, expiresAt, createdAt)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(userId, purpose) DO UPDATE SET
		   id = excluded.id,
		   tokenHash = excluded.tokenHash,
		   email = excluded.email,
		   currentEmail = excluded.currentEmail,
		   sessionVersion = excluded.sessionVersion,
		   expiresAt = excluded.expiresAt,
		   createdAt = excluded.createdAt`,
		uuid.NewString(), userID, purpose, tokenHash, targetEmail, currentEmail, sessionVersion, expiresAt, now); err != nil {
		return fmt.Errorf("保存账号令牌失败: %w", err)
	}

	if a.mailSend == nil {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE tokenHash = ?`, tokenHash)
		return errMailUnavailable
	}
	msg, err := a.recoveryMailMessage(purpose, targetEmail, token)
	if err != nil {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE tokenHash = ?`, tokenHash)
		return err
	}
	if err := a.mailSend(ctx, msg); err != nil {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE tokenHash = ?`, tokenHash)
		return err
	}
	return nil
}

func (a *App) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email string `json:"email"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}
	email, err := normalizeEmail(payload.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "请输入有效的邮箱地址")
		return
	}

	if allowed, retryAfter := a.recoverySendLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}
	if allowed, retryAfter := a.recoveryMailLimiter.allow("email:" + email); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	if !a.mailAvailable() {
		writeError(w, http.StatusServiceUnavailable, "MAIL_UNAVAILABLE", "邮件功能尚未配置，请联系管理员恢复账号")
		return
	}

	// 账号查询与投递都放到后台有界队列：公开接口的响应与耗时不能泄露邮箱是否已注册。
	if !a.enqueueRecoveryMail(mailJob{to: email, purpose: accountTokenPurposePasswordReset}) {
		writeMailLog("找回密码任务入队失败（队列已满或正在关闭）", nil)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := a.recoveryVerifyLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}
	if err := validatePassword(payload.Password); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", "新密码至少 6 位")
		return
	}

	switch err := a.resetAccountPassword(r.Context(), payload.Token, payload.Password); {
	case errors.Is(err, errInvalidAccountToken):
		writeError(w, http.StatusBadRequest, "INVALID_TOKEN", "链接无效或已过期，请重新申请")
		return
	case errors.Is(err, errSamePassword):
		writeError(w, http.StatusBadRequest, "SAME_PASSWORD", "新密码不能与原密码相同")
		return
	case errors.Is(err, errBcryptBusy):
		writeRateLimited(w, passwordChangeRetryAfter)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "重置密码失败，请稍后重试")
		return
	}

	a.clearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetAccountPassword 消费一次性令牌并重置密码。
//
// 校验：用途必须是 PASSWORD_RESET、未过期、账号邮箱与签发时一致、
// sessionVersion 未变化、账号未禁用；新密码不能与旧密码相同。
// 事务内条件删除令牌 + 条件更新账号 + 撤销全部 API Key + 清空该用户令牌，
// 并发消费只有一个赢家（另一个得到 0 行 → INVALID_TOKEN）。
func (a *App) resetAccountPassword(ctx context.Context, rawToken, password string) error {
	tokenHash, err := accountTokenHash(rawToken)
	if err != nil {
		return err
	}

	var (
		tokenID          string
		userID           int64
		purpose          string
		tokenEmail       string
		currentEmail     string
		tokenVersion     int64
		expiresAt        string
		userEmail        string
		userVersion      int64
		disabledAt       sql.NullString
		userPasswordHash string
	)
	err = a.db.QueryRowContext(ctx,
		`SELECT t.id, t.userId, t.purpose, t.email, t.currentEmail, t.sessionVersion, t.expiresAt,
		        u.email, u.sessionVersion, u.disabledAt, u.passwordHash
		 FROM "AccountToken" t JOIN "User" u ON u.id = t.userId
		 WHERE t.tokenHash = ?`, tokenHash).
		Scan(&tokenID, &userID, &purpose, &tokenEmail, &currentEmail, &tokenVersion, &expiresAt,
			&userEmail, &userVersion, &disabledAt, &userPasswordHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errInvalidAccountToken
	case err != nil:
		return err
	}

	if purpose != accountTokenPurposePasswordReset ||
		expiresAt <= coreNow() ||
		disabledAt.Valid ||
		userEmail != currentEmail ||
		userVersion != tokenVersion {
		return errInvalidAccountToken
	}

	// bcrypt 比较与重新哈希都在事务外：数据库只有单连接，不能在慢操作期间占着它。
	if err := a.comparePassword(userPasswordHash, password); err == nil {
		return errSamePassword
	} else if errors.Is(err, errBcryptBusy) {
		return err
	}

	newHash, err := a.hashPassword(password)
	if err != nil {
		return err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := coreNow()
	result, err := tx.ExecContext(ctx,
		`DELETE FROM "AccountToken"
		 WHERE tokenHash = ? AND purpose = ? AND expiresAt > ?`,
		tokenHash, accountTokenPurposePasswordReset, now)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errInvalidAccountToken
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE "User"
		 SET passwordHash = ?, mustChangePassword = 0, sessionVersion = sessionVersion + 1
		 WHERE id = ? AND email = ? AND sessionVersion = ? AND disabledAt IS NULL`,
		string(newHash), userID, currentEmail, tokenVersion)
	if err != nil {
		return err
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errInvalidAccountToken
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE "ApiKey" SET revokedAt = ? WHERE userId = ? AND revokedAt IS NULL`,
		now, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE userId = ?`, userID); err != nil {
		return err
	}

	return tx.Commit()
}
