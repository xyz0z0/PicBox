package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (a *App) registerAuthRoutes() {
	a.mux.HandleFunc("POST /api/auth/register", a.handleRegister)
	a.mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	a.mux.HandleFunc("GET /api/auth/me", a.handleMe)
	a.mux.HandleFunc("POST /api/auth/logout", a.handleLogout)

	// 原版 UI 使用的新路径；语义见 auth_change_password.go。
	a.mux.HandleFunc("POST /api/auth/change-password", a.handleChangePasswordNew)
	// 保留的旧内部入口（Go 预览页仍在使用），语义不变：成功后清除 Cookie 并撤销 API Key。
	a.mux.HandleFunc("POST /api/auth/password", a.handleChangePassword)

	a.mux.HandleFunc("POST /api/auth/forgot-password", a.handleForgotPassword)
	a.mux.HandleFunc("POST /api/auth/reset-password", a.handleResetPassword)
	a.mux.HandleFunc("POST /api/auth/change-email", a.handleChangeEmail)
	a.mux.HandleFunc("POST /api/auth/change-email/confirm", a.handleConfirmChangeEmail)

	a.mux.HandleFunc("GET /api/apikeys", a.handleListAPIKeys)
	a.mux.HandleFunc("POST /api/apikeys", a.handleCreateAPIKey)
	a.mux.HandleFunc("DELETE /api/apikeys/{id}", a.handleRevokeAPIKey)

	a.mux.HandleFunc("GET /api/health", a.handleHealth)

	// 未实现的 API 必须返回 JSON，而不是网页或纯文本。
	a.mux.HandleFunc("/api/", a.handleUnknownAPI)
	a.mux.HandleFunc("/api", a.handleUnknownAPI)
}

type credentialsPayload struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := a.loginLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var payload credentialsPayload
	if !decodeJSONBody(w, r, &payload) {
		return
	}

	email, emailErr := normalizeEmail(payload.Email)
	identifier := email
	if emailErr != nil {
		identifier = strings.ToLower(strings.TrimSpace(payload.Email))
	}
	if identifier == "" || payload.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "邮箱或密码错误")
		return
	}

	if allowed, retryAfter := a.loginEmailLimiter.allow("email:" + identifier); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	var (
		record userRecord
		err    error
	)
	if emailErr == nil {
		record, err = a.loadUserByEmail(r.Context(), email)
	} else {
		err = errNoUser
	}
	if err != nil && !errors.Is(err, errNoUser) {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "登录失败，请稍后重试")
		return
	}

	// 账号不存在时同样执行一次 cost=10 的 bcrypt 比较。
	hash := dummyPasswordHash
	if err == nil {
		hash = record.passwordHash
	}
	compareErr := a.comparePassword(hash, payload.Password)
	if errors.Is(compareErr, errBcryptBusy) {
		writeRateLimited(w, time.Second)
		return
	}
	if err != nil || compareErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "邮箱或密码错误")
		return
	}
	if record.disabled {
		writeError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用")
		return
	}

	// 管理员登录必须落审计；审计写不进去就不放行会话。
	if record.user.Role == roleAdmin {
		if err := a.auditAdminLogin(r.Context(), record.user.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "登录失败，请稍后重试")
			return
		}
	}

	token, err := a.signToken(record.user.ID, record.user.SessionVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "签发登录凭证失败")
		return
	}
	a.setAuthCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": record.user})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	user, failure := a.resolveUser(r, false)
	if failure != nil {
		// /api/auth/me 沿用顶层 code/message 错误格式。
		writeJSON(w, failure.status, map[string]string{
			"code":    failure.code,
			"message": failure.message,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearAuthCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type apiKeyView struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	RevokedAt  *string `json:"revokedAt"`
}

func (a *App) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, name, prefix, createdAt, lastUsedAt, revokedAt
		 FROM "ApiKey" WHERE userId = ? AND revokedAt IS NULL
		 ORDER BY createdAt DESC, id DESC`, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "读取 API Key 失败")
		return
	}
	defer rows.Close()

	keys := make([]apiKeyView, 0)
	for rows.Next() {
		var (
			key        apiKeyView
			lastUsedAt sql.NullString
			revokedAt  sql.NullString
		)
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &lastUsedAt, &revokedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "读取 API Key 失败")
			return
		}
		if lastUsedAt.Valid {
			value := lastUsedAt.String
			key.LastUsedAt = &value
		}
		if revokedAt.Valid {
			value := revokedAt.String
			key.RevokedAt = &value
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "读取 API Key 失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": keys})
}

func (a *App) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	var payload struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" || len(name) > maxAPIKeyNameBytes {
		writeError(w, http.StatusBadRequest, "INVALID_NAME", "请提供 1~100 字节的 Key 名称")
		return
	}

	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "生成 API Key 失败")
		return
	}
	rawKey := "pk_" + hex.EncodeToString(random)
	keyID := uuid.NewString()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO "ApiKey" (id, userId, name, keyHash, prefix, createdAt, lastUsedAt, revokedAt)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, NULL)`,
		keyID, user.ID, name, hashAPIKey(rawKey), rawKey[:10], createdAt); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "保存 API Key 失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"key": map[string]any{
			"id":        keyID,
			"name":      name,
			"prefix":    rawKey[:10],
			"rawKey":    rawKey,
			"createdAt": createdAt,
		},
	})
}

func (a *App) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}

	keyID := strings.TrimSpace(r.PathValue("id"))
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "缺少 API Key ID")
		return
	}

	result, err := a.db.ExecContext(r.Context(),
		`UPDATE "ApiKey" SET revokedAt = ?
		 WHERE id = ? AND userId = ? AND revokedAt IS NULL`,
		time.Now().UTC().Format(time.RFC3339), keyID, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "撤销 API Key 失败")
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "撤销 API Key 失败")
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "KEY_NOT_FOUND", "API Key 不存在或已撤销")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
