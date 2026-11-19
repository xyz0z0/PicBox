package server

// 管理端接口回归的共享测试辅助：真实临时目录 + 真实 SQLite + httptest。
// 前缀统一用 adminTest，避免与 core/images worker 的同包测试辅助重名。

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type adminTestEnv struct {
	app     *App
	handler http.Handler
}

type adminTestAuth struct {
	cookie *http.Cookie
	bearer string
}

func adminTestNewApp(t *testing.T, cfg Config) *adminTestEnv {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	// core 已在 app.go 的 New 里挂载 registerAdminRoutes()；这里再显式调用一次，
	// 既证明注册函数幂等（重复调用不会 panic），也让测试不依赖挂载顺序。
	app.registerAdminRoutes()
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() 失败: %v", err)
		}
	})
	return &adminTestEnv{app: app, handler: app.Handler()}
}

func (e *adminTestEnv) do(t *testing.T, method, target string, body io.Reader, auth adminTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if auth.cookie != nil {
		request.AddCookie(auth.cookie)
	}
	if auth.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+auth.bearer)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

func (e *adminTestEnv) doJSON(t *testing.T, method, target string, payload any, auth adminTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	return e.do(t, method, target, body, auth)
}

func adminTestCookieAuth(cookie *http.Cookie) adminTestAuth { return adminTestAuth{cookie: cookie} }

func adminTestJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON (status=%d): %v: %s", recorder.Code, err, recorder.Body.String())
	}
	return body
}

func adminTestErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := adminTestJSON(t, recorder)
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	code, _ := errorBody["code"].(string)
	return code
}

func adminTestNumber(t *testing.T, body map[string]any, key string) int64 {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("响应缺少字段 %s: %v", key, body)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %v", key, value)
	}
	return int64(number)
}

func adminTestBool(t *testing.T, body map[string]any, key string) bool {
	t.Helper()
	value, ok := body[key].(bool)
	if !ok {
		t.Fatalf("字段 %s 不是布尔: %v", key, body[key])
	}
	return value
}

func adminTestString(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(string)
	if !ok {
		t.Fatalf("字段 %s 不是字符串: %v", key, body[key])
	}
	return value
}

// adminTestCollectCookies 找注册/登录响应里的会话 Cookie。
func adminTestCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == authCookieName {
			return cookie
		}
	}
	t.Fatalf("响应没有 %s Cookie (status=%d)", authCookieName, recorder.Code)
	return nil
}

// adminTestRegisterAdmin 通过真实注册接口创建第一个账号（自动成为管理员）。
func (e *adminTestEnv) registerAdmin(t *testing.T, email, password string) *http.Cookie {
	t.Helper()
	recorder := e.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": email, "password": password}, adminTestAuth{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("注册管理员失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return adminTestCookie(t, recorder)
}

// adminTestUserPassword 是 addUser 插入的普通账号密码，供登录相关断言使用。
const adminTestUserPassword = "plain-user-pass"

// addUser 直接插入一个普通账号并签发会话 Cookie（不经过注册/限流）。
func (e *adminTestEnv) addUser(t *testing.T, email string) (int64, *http.Cookie) {
	t.Helper()
	hash, err := e.app.hashPassword(adminTestUserPassword)
	if err != nil {
		t.Fatalf("生成测试密码哈希失败: %v", err)
	}
	result, err := e.app.db.Exec(`INSERT INTO "User" (email, username, passwordHash, role, sessionVersion, mustChangePassword, createdAt)
		VALUES (?, ?, ?, ?, 0, 0, ?)`,
		email, "user_"+uuid.NewString(), string(hash), roleUser, coreNow())
	if err != nil {
		t.Fatalf("插入测试用户失败: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("读取测试用户 id 失败: %v", err)
	}
	token, err := e.app.signToken(id, 0)
	if err != nil {
		t.Fatalf("签发测试用户令牌失败: %v", err)
	}
	return id, &http.Cookie{Name: authCookieName, Value: token}
}

func (e *adminTestEnv) addAPIKey(t *testing.T, userID int64) string {
	t.Helper()
	raw := "admin-test-key-" + uuid.NewString()
	if _, err := e.app.db.Exec(`INSERT INTO "ApiKey" (id, userId, name, keyHash, prefix, createdAt)
		VALUES (?, ?, 'test', ?, 'adm', ?)`,
		uuid.NewString(), userID, hashAPIKey(raw), coreNow()); err != nil {
		t.Fatalf("插入测试 API Key 失败: %v", err)
	}
	return raw
}

type adminTestImageOptions struct {
	ID             string
	CreatedAt      string
	Size           int64
	IsVisible      int
	DeletedAt      *string
	DeletedByAdmin int
}

// addImage 直接插入一张图片记录（管理端接口不读取文件字节，因此不需要真实文件）。
func (e *adminTestEnv) addImage(t *testing.T, uploaderID int64, name string, opts adminTestImageOptions) string {
	t.Helper()
	if opts.ID == "" {
		opts.ID = uuid.NewString()
	}
	if opts.CreatedAt == "" {
		opts.CreatedAt = imagesNow()
	}
	if opts.Size == 0 {
		opts.Size = 1024
	}
	if _, err := e.app.db.Exec(`INSERT INTO "Image"
		(id, uploaderId, originalName, originalMimeType, size, sha256, width, height,
		 originalPath, thumbPath, createdAt, updatedAt, deletedAt, deletedByAdmin, isVisible)
		VALUES (?, ?, ?, 'image/png', ?, '', 10, 10, ?, ?, ?, ?, ?, ?, ?)`,
		opts.ID, uploaderID, name, opts.Size,
		"originals/"+opts.ID+".png", "thumbs/"+opts.ID+".webp",
		opts.CreatedAt, opts.CreatedAt, opts.DeletedAt, opts.DeletedByAdmin, opts.IsVisible); err != nil {
		t.Fatalf("插入测试图片失败: %v", err)
	}
	return opts.ID
}

// addActivationCode 直接插入激活码；rawCode 用于验证注册消费（会按同一规范哈希）。
func (e *adminTestEnv) addActivationCode(t *testing.T, rawCode string, createdByID int64, createdAt string, expiresAt *string, usedAt, revokedAt *string) string {
	t.Helper()
	id := uuid.NewString()
	normalized := normalizeActivationCode(rawCode)
	var expires, used, revoked any
	if expiresAt != nil {
		expires = *expiresAt
	}
	if usedAt != nil {
		used = *usedAt
	}
	if revokedAt != nil {
		revoked = *revokedAt
	}
	if _, err := e.app.db.Exec(`INSERT INTO "ActivationCode"
		(id, codeHash, maskedCode, note, createdById, createdAt, expiresAt, usedAt, revokedAt)
		VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		id, hashActivationCode(normalized), adminMaskActivationCode(normalized), createdByID,
		createdAt, expires, used, revoked); err != nil {
		t.Fatalf("插入测试激活码失败: %v", err)
	}
	return id
}

type adminTestAuditRow struct {
	ID           int64
	ActorID      int64
	Action       string
	TargetUserID sql.NullInt64
	Detail       sql.NullString
	CreatedAt    string
}

func (e *adminTestEnv) auditRows(t *testing.T) []adminTestAuditRow {
	t.Helper()
	rows, err := e.app.db.Query(`SELECT id, actorId, action, targetUserId, detail, createdAt FROM "AuditLog" ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("读取审计行失败: %v", err)
	}
	defer rows.Close()
	entries := make([]adminTestAuditRow, 0, 8)
	for rows.Next() {
		var row adminTestAuditRow
		if err := rows.Scan(&row.ID, &row.ActorID, &row.Action, &row.TargetUserID, &row.Detail, &row.CreatedAt); err != nil {
			t.Fatalf("扫描审计行失败: %v", err)
		}
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取审计行失败: %v", err)
	}
	return entries
}

func (e *adminTestEnv) auditRowsFor(t *testing.T, action string) []adminTestAuditRow {
	t.Helper()
	filtered := make([]adminTestAuditRow, 0, 4)
	for _, row := range e.auditRows(t) {
		if row.Action == action {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func adminTestMustOne(t *testing.T, rows []adminTestAuditRow, action string) adminTestAuditRow {
	t.Helper()
	matched := make([]adminTestAuditRow, 0, 2)
	for _, row := range rows {
		if row.Action == action {
			matched = append(matched, row)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("期望恰好 1 条 %s 审计，实际 %d 条: %v", action, len(matched), rows)
	}
	return matched[0]
}

func (e *adminTestEnv) userDisabledAt(t *testing.T, userID int64) sql.NullString {
	t.Helper()
	var disabled sql.NullString
	if err := e.app.db.QueryRow(`SELECT disabledAt FROM "User" WHERE id = ?`, userID).Scan(&disabled); err != nil {
		t.Fatalf("读取账号状态失败: %v", err)
	}
	return disabled
}

func (e *adminTestEnv) login(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return e.doJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"email": email, "password": password}, adminTestAuth{})
}

func adminTestListItems(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("字段 %s 不是数组: %v", key, body[key])
	}
	items := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("字段 %s 含非对象元素: %v", key, raw)
		}
		items = append(items, item)
	}
	return items
}
