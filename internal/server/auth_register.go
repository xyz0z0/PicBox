package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// 注册激活码不区分大小写与分隔符：注册只保存 sha256 哈希（激活码本身是高熵随机串）。
var (
	errActivationCodeRequired = errors.New("需要激活码")
	errInvalidActivationCode  = errors.New("激活码无效")
	errActivationCodeConflict = errors.New("激活码已被使用")
	errRegistrationConflict   = errors.New("注册冲突")
)

// normalizeActivationCode 去掉分隔符并转大写（与原版 lib/activationCode.ts 一致）。
func normalizeActivationCode(value string) string {
	upper := strings.ToUpper(value)
	var builder strings.Builder
	for _, char := range upper {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// hashActivationCode 只保存哈希，不落库完整激活码。
func hashActivationCode(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

type registerPayload struct {
	Email          string `json:"email"`
	Password       string `json:"password"`
	ActivationCode string `json:"activationCode"`
}

// handleRegister 实现 POST /api/auth/register。
//
// 语义与原版一致：首个账号免激活码且成为管理员；此后每个账号必须消费一个
// 未使用、未撤销、未过期的激活码，并在同一事务内写入 ACTIVATION_CODE_USED 审计。
// 密码哈希（bcrypt）在事务外完成，事务内只用 tx 查询，避免单连接数据库自锁。
func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := a.registerLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload registerPayload
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	email, err := normalizeEmail(payload.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "请输入有效的邮箱地址")
		return
	}
	if err := validatePassword(payload.Password); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}

	passwordHash, err := a.hashPassword(payload.Password)
	if err != nil {
		if errors.Is(err, errBcryptBusy) {
			writeRateLimited(w, time.Second)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "注册失败，请稍后重试")
		return
	}

	user, err := a.createUserWithActivationCode(r.Context(), email, passwordHash, payload.ActivationCode)
	switch {
	case errors.Is(err, errActivationCodeRequired):
		writeError(w, http.StatusBadRequest, "ACTIVATION_CODE_REQUIRED", "请输入激活码：第一个账号无需激活码，之后需要管理员在后台生成")
		return
	case errors.Is(err, errInvalidActivationCode):
		writeError(w, http.StatusBadRequest, "INVALID_ACTIVATION_CODE", "激活码无效或已使用")
		return
	case errors.Is(err, errActivationCodeConflict):
		writeError(w, http.StatusConflict, "ACTIVATION_CODE_CONFLICT", "激活码已被使用，请重试")
		return
	case errors.Is(err, errRegistrationConflict):
		writeError(w, http.StatusConflict, "REGISTRATION_CONFLICT", "注册冲突，请重试")
		return
	case err != nil:
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已注册")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "注册失败，请稍后重试")
		return
	}

	token, err := a.signToken(user.ID, user.SessionVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "签发登录凭证失败")
		return
	}
	a.setAuthCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user})
}

// createUserWithActivationCode 在单事务内完成注册：判定首个账号、消费激活码、
// 写 ACTIVATION_CODE_USED 审计。返回的错误都已归一，调用方只负责映射 HTTP 状态。
func (a *App) createUserWithActivationCode(ctx context.Context, email string, passwordHash []byte, rawCode string) (*User, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := coreNow()

	var userCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM "User"`).Scan(&userCount); err != nil {
		return nil, err
	}

	if userCount == 0 {
		// 首个账号免码并成为管理员：事务内二次确认（上面的计数查询在同一事务内），
		// 单连接数据库下并发注册也只会有一个赢家。
		user, err := insertRegisteredUser(ctx, tx, email, passwordHash, roleAdmin, now)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return user, nil
	}

	normalized := normalizeActivationCode(rawCode)
	if normalized == "" {
		return nil, errActivationCodeRequired
	}
	codeHash := hashActivationCode(normalized)

	var (
		codeID    string
		expiresAt sql.NullString
		usedAt    sql.NullString
		revokedAt sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, expiresAt, usedAt, revokedAt FROM "ActivationCode" WHERE codeHash = ?`,
		codeHash).Scan(&codeID, &expiresAt, &usedAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, errInvalidActivationCode
	case err != nil:
		return nil, err
	}
	if usedAt.Valid || revokedAt.Valid {
		return nil, errInvalidActivationCode
	}
	if expiresAt.Valid && expiresAt.String <= now {
		return nil, errInvalidActivationCode
	}

	user, err := insertRegisteredUser(ctx, tx, email, passwordHash, roleUser, now)
	if err != nil {
		return nil, err
	}

	// 条件更新是并发下唯一的消费点：抢不到就整体回滚（刚创建的用户也不会留下）。
	result, err := tx.ExecContext(ctx,
		`UPDATE "ActivationCode" SET usedAt = ?, usedById = ?
		 WHERE id = ? AND usedAt IS NULL AND revokedAt IS NULL
		   AND (expiresAt IS NULL OR expiresAt > ?)`,
		now, user.ID, codeID, now)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, errActivationCodeConflict
	}

	if err := auditWrite(ctx, tx, user.ID, auditActionActivationCodeUsed, nil, "codeId="+codeID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

func insertRegisteredUser(ctx context.Context, tx *sql.Tx, email string, passwordHash []byte, role, now string) (*User, error) {
	username := newInternalUsername()
	result, err := tx.ExecContext(ctx,
		`INSERT INTO "User" (email, username, passwordHash, role, disabledAt, sessionVersion, mustChangePassword, quotaBytes, createdAt)
		 VALUES (?, ?, ?, ?, NULL, 0, 0, NULL, ?)`,
		email, username, string(passwordHash), role, now)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{
		ID:                 id,
		Email:              email,
		Username:           username,
		Role:               role,
		SessionVersion:     0,
		MustChangePassword: false,
	}, nil
}

// auditAdminLogin 在管理员成功登录后写 ADMIN_LOGIN 审计。
// 审计写不进去时登录失败（不签发会话）：管理员会话必须可审计。
func (a *App) auditAdminLogin(ctx context.Context, actorID int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := auditWrite(ctx, tx, actorID, auditActionAdminLogin, nil, ""); err != nil {
		return err
	}
	return tx.Commit()
}
