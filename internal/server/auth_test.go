package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---- shared helpers for the core (app/config/db/auth) tests ----
// 前缀刻意保持唯一，避免与 images/web worker 的同包测试辅助函数重名。

type coreResponse struct {
	recorder *httptest.ResponseRecorder
	status   int
	body     map[string]any
	cookies  []*http.Cookie
}

func coreTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() 失败: %v", err)
		}
	})
	return app
}

func coreDo(t *testing.T, handler http.Handler, method, target string, payload any, headers map[string]string) *coreResponse {
	t.Helper()

	var body io.Reader
	switch value := payload.(type) {
	case nil:
	case string:
		body = strings.NewReader(value)
	case []byte:
		body = bytes.NewReader(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	request := httptest.NewRequest(method, target, body)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := &coreResponse{recorder: recorder, status: recorder.Code}
	if cookies := recorder.Result().Cookies(); len(cookies) > 0 {
		response.cookies = cookies
	}
	raw := recorder.Body.Bytes()
	if len(raw) > 0 && strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &response.body); err != nil {
			t.Fatalf("响应不是合法 JSON (%s %s): %v: %s", method, target, err, raw)
		}
	}
	return response
}

func coreCookieHeader(cookie *http.Cookie) map[string]string {
	return map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
}

func coreErrorCode(t *testing.T, response *coreResponse) string {
	t.Helper()
	errorBody, ok := response.body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: status=%d body=%v", response.status, response.body)
	}
	code, _ := errorBody["code"].(string)
	return code
}

func coreRegister(t *testing.T, app *App, email, password string) (*coreResponse, *http.Cookie) {
	t.Helper()
	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/register",
		map[string]string{"email": email, "password": password}, nil)
	if response.status != http.StatusOK {
		t.Fatalf("注册失败: status=%d body=%v", response.status, response.body)
	}
	for _, cookie := range response.cookies {
		if cookie.Name == app.cfg.AuthCookieName {
			return response, cookie
		}
	}
	t.Fatalf("注册响应没有 %s Cookie: %v", app.cfg.AuthCookieName, response.cookies)
	return nil, nil
}

func coreUserCount(t *testing.T, app *App) (total int, admins int) {
	t.Helper()
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM "User"`).Scan(&total); err != nil {
		t.Fatalf("统计用户失败: %v", err)
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM "User" WHERE role = 'ADMIN'`).Scan(&admins); err != nil {
		t.Fatalf("统计管理员失败: %v", err)
	}
	return total, admins
}

func coreFileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func coreDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %s 失败: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestConcurrentRegistrationOnlyCreatesOneAdministrator(t *testing.T) {
	app := coreTestApp(t, Config{})

	const attempts = 8
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		statuses  = make([]int, attempts)
		start     = make(chan struct{})
	)
	for i := 0; i < attempts; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			recorder := httptest.NewRecorder()
			body := strings.NewReader(`{"email":"user` + string(rune('a'+index)) + `@example.com","password":"secret123"}`)
			request := httptest.NewRequest(http.MethodPost, "/api/auth/register", body)
			request.Header.Set("Content-Type", "application/json")
			app.Handler().ServeHTTP(recorder, request)
			mu.Lock()
			statuses[index] = recorder.Code
			mu.Unlock()
		}(i)
	}
	close(start)
	waitGroup.Wait()

	successes := 0
	codeRequired := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
			// 首个账号之外的并发注册都必须带激活码。
			codeRequired++
		default:
			t.Fatalf("并发注册出现意外状态: %v", statuses)
		}
	}
	if successes != 1 || codeRequired != attempts-1 {
		t.Fatalf("并发注册应当只有一个成功: %v", statuses)
	}

	total, admins := coreUserCount(t, app)
	if total != 1 || admins != 1 {
		t.Fatalf("并发注册产生了 %d 个用户 / %d 个管理员，期望 1/1", total, admins)
	}
}

func coreSignRawJWT(t *testing.T, secret []byte, payload string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signingInput := header + "." + body
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func coreMeWithToken(t *testing.T, app *App, token string) *coreResponse {
	t.Helper()
	return coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil,
		map[string]string{"Cookie": app.cfg.AuthCookieName + "=" + token})
}

func TestParseTokenRequiresExplicitSessionVersion(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "claims@example.com", "secret123")

	// 合法 0 必须被接受（刚注册的账号 sessionVersion 就是 0）。
	validZero := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie))
	if validZero.status != http.StatusOK {
		t.Fatalf("合法 sessionVersion=0 应当通过: %d %v", validZero.status, validZero.body)
	}

	exp := time.Now().Add(time.Hour).Unix()
	payloads := map[string]string{
		"缺少 sessionVersion":  fmt.Sprintf(`{"userId":1,"exp":%d}`, exp),
		"负数 sessionVersion":  fmt.Sprintf(`{"userId":1,"sessionVersion":-1,"exp":%d}`, exp),
		"小数 sessionVersion":  fmt.Sprintf(`{"userId":1,"sessionVersion":1.5,"exp":%d}`, exp),
		"字符串 sessionVersion": fmt.Sprintf(`{"userId":1,"sessionVersion":"0","exp":%d}`, exp),
		"布尔 sessionVersion":  fmt.Sprintf(`{"userId":1,"sessionVersion":true,"exp":%d}`, exp),
		"缺少 userId":          fmt.Sprintf(`{"sessionVersion":0,"exp":%d}`, exp),
		"零 userId":           fmt.Sprintf(`{"userId":0,"sessionVersion":0,"exp":%d}`, exp),
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			token := coreSignRawJWT(t, app.secret, payload)
			request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			request.Header.Set("Cookie", app.cfg.AuthCookieName+"="+token)
			recorder := httptest.NewRecorder()
			app.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s 应当 401，实际 %d: %s", name, recorder.Code, recorder.Body.String())
			}
		})
	}

	// 重复 JSON 字段沿用 encoding/json 的“最后一次生效”行为。
	duplicate := fmt.Sprintf(`{"userId":1,"sessionVersion":-1,"sessionVersion":0,"exp":%d}`, exp)
	duplicateResponse := coreMeWithToken(t, app, coreSignRawJWT(t, app.secret, duplicate))
	if duplicateResponse.status != http.StatusOK {
		t.Fatalf("重复 sessionVersion 字段应按库行为取最后值: %d %v", duplicateResponse.status, duplicateResponse.body)
	}
}

func TestCustomAuthCookieName(t *testing.T) {
	const customName = "picbox_custom_token"
	app := coreTestApp(t, Config{AuthCookieName: customName})

	response, cookie := coreRegister(t, app, "cookie@example.com", "secret123")
	if cookie.Name != customName {
		t.Fatalf("注册应使用配置的 Cookie 名: %q", cookie.Name)
	}
	for _, candidate := range response.cookies {
		if candidate.Name == "picbox_token" {
			t.Fatalf("不应再下发默认的 picbox_token Cookie")
		}
	}

	me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie))
	if me.status != http.StatusOK {
		t.Fatalf("自定义 Cookie 名应当可认证: %d %v", me.status, me.body)
	}

	// 同样的 JWT 放在默认 Cookie 名下不应被接受。
	wrongName := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil,
		map[string]string{"Cookie": "picbox_token=" + cookie.Value})
	if wrongName.status != http.StatusUnauthorized {
		t.Fatalf("认证只能读取配置的 Cookie 名: %d %v", wrongName.status, wrongName.body)
	}

	logout := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/logout", nil, coreCookieHeader(cookie))
	cleared := false
	for _, candidate := range logout.cookies {
		if candidate.Name == customName && candidate.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("退出必须清除配置的 Cookie 名: %v", logout.cookies)
	}
}

func TestRegisterRequiresActivationCodeAfterFirstUser(t *testing.T) {
	app := coreTestApp(t, Config{})
	response, cookie := coreRegister(t, app, "first@example.com", "secret123")
	if cookie.HttpOnly != true || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("Cookie 属性不符合契约: %+v", cookie)
	}
	user, _ := response.body["user"].(map[string]any)
	if user["role"] != "ADMIN" {
		t.Fatalf("首个账号应当是管理员: %v", user)
	}
	if _, leaked := user["sessionVersion"]; leaked {
		t.Fatalf("user 视图不应暴露 sessionVersion: %v", user)
	}

	me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie))
	if me.status != http.StatusOK {
		t.Fatalf("me 失败: %d %v", me.status, me.body)
	}

	second := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/register",
		map[string]string{"email": "second@example.com", "password": "secret123"}, nil)
	if second.status != http.StatusBadRequest || coreErrorCode(t, second) != "ACTIVATION_CODE_REQUIRED" {
		t.Fatalf("后续注册必须要求激活码: %d %v", second.status, second.body)
	}
	if total, _ := coreUserCount(t, app); total != 1 {
		t.Fatalf("缺少激活码时不得写入用户: %d", total)
	}
}

func TestLoginRejectsBadPasswordAndUnknownEmail(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "user@example.com", "secret123")

	wrong := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "user@example.com", "password": "wrong-password"}, nil)
	if wrong.status != http.StatusBadRequest || coreErrorCode(t, wrong) != "INVALID_CREDENTIALS" {
		t.Fatalf("错误密码应当拒绝: %d %v", wrong.status, wrong.body)
	}

	unknown := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "missing@example.com", "password": "secret123"}, nil)
	if unknown.status != http.StatusBadRequest || coreErrorCode(t, unknown) != "INVALID_CREDENTIALS" {
		t.Fatalf("未知账号应当返回同等错误: %d %v", unknown.status, unknown.body)
	}

	ok := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "USER@example.com ", "password": "secret123"}, nil)
	if ok.status != http.StatusOK {
		t.Fatalf("规范化邮箱后登录应当成功: %d %v", ok.status, ok.body)
	}

	logout := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/logout", nil, nil)
	if logout.status != http.StatusOK {
		t.Fatalf("退出失败: %d", logout.status)
	}
	cleared := false
	for _, cookie := range logout.cookies {
		if cookie.Name == app.cfg.AuthCookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("退出应当清除 Cookie: %v", logout.cookies)
	}
}

func TestDisabledAccountAndSessionVersion(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "disabled@example.com", "secret123")

	if _, err := app.db.Exec(`UPDATE "User" SET disabledAt = ? WHERE email = ?`,
		"2026-01-01T00:00:00Z", "disabled@example.com"); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}

	me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie))
	if me.status != http.StatusForbidden {
		t.Fatalf("禁用账号的 me 应当 403: %d %v", me.status, me.body)
	}
	if me.body["code"] != "ACCOUNT_DISABLED" {
		t.Fatalf("/api/auth/me 错误应为顶层 code: %v", me.body)
	}

	login := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "disabled@example.com", "password": "secret123"}, nil)
	if login.status != http.StatusForbidden || coreErrorCode(t, login) != "ACCOUNT_DISABLED" {
		t.Fatalf("禁用账号登录应当 403: %d %v", login.status, login.body)
	}

	// sessionVersion 递增后旧 Cookie 失效，重新登录可拿到新版本。
	app2 := coreTestApp(t, Config{})
	_, cookie2 := coreRegister(t, app2, "version@example.com", "secret123")
	if _, err := app2.db.Exec(`UPDATE "User" SET sessionVersion = 1 WHERE email = ?`,
		"version@example.com"); err != nil {
		t.Fatalf("递增 sessionVersion 失败: %v", err)
	}
	stale := coreDo(t, app2.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie2))
	if stale.status != http.StatusUnauthorized {
		t.Fatalf("旧 sessionVersion 应当 401: %d %v", stale.status, stale.body)
	}

	fresh := coreDo(t, app2.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "version@example.com", "password": "secret123"}, nil)
	if fresh.status != http.StatusOK {
		t.Fatalf("重新登录失败: %d %v", fresh.status, fresh.body)
	}
	var freshCookie *http.Cookie
	for _, candidate := range fresh.cookies {
		if candidate.Name == app2.cfg.AuthCookieName {
			freshCookie = candidate
		}
	}
	if freshCookie == nil {
		t.Fatalf("重新登录没有下发 Cookie: %v", fresh.cookies)
	}
	meAgain := coreDo(t, app2.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(freshCookie))
	if meAgain.status != http.StatusOK {
		t.Fatalf("新 Cookie 应当有效: %d %v", meAgain.status, meAgain.body)
	}
}

func TestAPIKeyLifecycleAndBearerRestrictions(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "keys@example.com", "secret123")
	cookieHeaders := coreCookieHeader(cookie)

	created := coreDo(t, app.Handler(), http.MethodPost, "/api/apikeys",
		map[string]string{"name": "ci"}, cookieHeaders)
	if created.status != http.StatusOK {
		t.Fatalf("创建 API Key 失败: %d %v", created.status, created.body)
	}
	key, ok := created.body["key"].(map[string]any)
	if !ok {
		t.Fatalf("创建响应缺少 key: %v", created.body)
	}
	rawKey, _ := key["rawKey"].(string)
	keyID, _ := key["id"].(string)
	if !strings.HasPrefix(rawKey, "pk_") || len(rawKey) != 51 || keyID == "" {
		t.Fatalf("API Key 格式不符合契约: %v", key)
	}
	if prefix, _ := key["prefix"].(string); prefix != rawKey[:10] {
		t.Fatalf("prefix 应当是 rawKey 前 10 位: %v", key)
	}

	var storedHash string
	if err := app.db.QueryRow(`SELECT keyHash FROM "ApiKey" WHERE id = ?`, keyID).Scan(&storedHash); err != nil {
		t.Fatalf("读取 keyHash 失败: %v", err)
	}
	if storedHash == rawKey || storedHash != hashAPIKey(rawKey) {
		t.Fatalf("数据库必须只保存 sha256 哈希")
	}

	listed := coreDo(t, app.Handler(), http.MethodGet, "/api/apikeys", nil, cookieHeaders)
	if listed.status != http.StatusOK {
		t.Fatalf("列出 API Key 失败: %d %v", listed.status, listed.body)
	}
	keys, _ := listed.body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("应列出 1 个 Key: %v", listed.body)
	}
	if _, leaked := keys[0].(map[string]any)["rawKey"]; leaked {
		t.Fatalf("列表不能返回 rawKey: %v", keys[0])
	}

	// Bearer 不能管理 Key。
	bearerManage := coreDo(t, app.Handler(), http.MethodGet, "/api/apikeys", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if bearerManage.status != http.StatusUnauthorized {
		t.Fatalf("Bearer 管理 Key 应当 401: %d %v", bearerManage.status, bearerManage.body)
	}

	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := app.authenticate(w, r, true)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": user.Email})
	})

	valid := coreDo(t, apiHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if valid.status != http.StatusOK || valid.body["email"] != "keys@example.com" {
		t.Fatalf("有效 API Key 认证失败: %d %v", valid.status, valid.body)
	}

	wrong := coreDo(t, apiHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer pk_not-a-real-key"})
	if wrong.status != http.StatusUnauthorized || coreErrorCode(t, wrong) != "INVALID_API_KEY" {
		t.Fatalf("错误 Key 应当 401: %d %v", wrong.status, wrong.body)
	}

	var lastUsed sql.NullString
	if err := app.db.QueryRow(`SELECT lastUsedAt FROM "ApiKey" WHERE id = ?`, keyID).Scan(&lastUsed); err != nil {
		t.Fatalf("读取 lastUsedAt 失败: %v", err)
	}
	if !lastUsed.Valid {
		t.Fatalf("有效 Key 认证后应记录 lastUsedAt")
	}

	revoked := coreDo(t, app.Handler(), http.MethodDelete, "/api/apikeys/"+keyID, nil, cookieHeaders)
	if revoked.status != http.StatusOK {
		t.Fatalf("撤销 Key 失败: %d %v", revoked.status, revoked.body)
	}
	afterRevoke := coreDo(t, apiHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if afterRevoke.status != http.StatusUnauthorized || coreErrorCode(t, afterRevoke) != "INVALID_API_KEY" {
		t.Fatalf("已撤销 Key 应当 401: %d %v", afterRevoke.status, afterRevoke.body)
	}

	listedAgain := coreDo(t, app.Handler(), http.MethodGet, "/api/apikeys", nil, cookieHeaders)
	if keys, _ := listedAgain.body["keys"].([]any); len(keys) != 0 {
		t.Fatalf("撤销后不应再列出该 Key: %v", listedAgain.body)
	}
}

func TestDisabledAccountRejectsAPIKey(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "keydisabled@example.com", "secret123")
	created := coreDo(t, app.Handler(), http.MethodPost, "/api/apikeys",
		map[string]string{"name": "ci"}, coreCookieHeader(cookie))
	key, _ := created.body["key"].(map[string]any)
	rawKey, _ := key["rawKey"].(string)
	keyID, _ := key["id"].(string)

	if _, err := app.db.Exec(`UPDATE "User" SET disabledAt = ? WHERE email = ?`,
		"2026-01-01T00:00:00Z", "keydisabled@example.com"); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}

	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := app.authenticate(w, r, true); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	response := coreDo(t, apiHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if response.status != http.StatusForbidden || coreErrorCode(t, response) != "ACCOUNT_DISABLED" {
		t.Fatalf("禁用账号的 Key 应当 403: %d %v", response.status, response.body)
	}
	_ = keyID
}

func TestAPIKeyCannotBeManagedByAnotherUser(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, ownerCookie := coreRegister(t, app, "owner@example.com", "secret123")
	created := coreDo(t, app.Handler(), http.MethodPost, "/api/apikeys",
		map[string]string{"name": "owner-key"}, coreCookieHeader(ownerCookie))
	key, _ := created.body["key"].(map[string]any)
	rawKey, _ := key["rawKey"].(string)
	keyID, _ := key["id"].(string)

	// 直接写入第二个普通账号（本阶段注册接口关闭，但账号隔离仍然必须成立）。
	hash, err := bcrypt.GenerateFromPassword([]byte("secret123"), bcryptCost)
	if err != nil {
		t.Fatalf("生成密码哈希失败: %v", err)
	}
	if _, err := app.db.Exec(
		`INSERT INTO "User" (email, username, passwordHash, role, disabledAt, sessionVersion, mustChangePassword, quotaBytes, createdAt)
		 VALUES (?, ?, ?, 'USER', NULL, 0, 0, NULL, ?)`,
		"intruder@example.com", "user_intruder", string(hash), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("插入第二账号失败: %v", err)
	}
	login := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "intruder@example.com", "password": "secret123"}, nil)
	if login.status != http.StatusOK {
		t.Fatalf("第二账号登录失败: %d %v", login.status, login.body)
	}
	var intruderCookie *http.Cookie
	for _, cookie := range login.cookies {
		if cookie.Name == app.cfg.AuthCookieName {
			intruderCookie = cookie
		}
	}
	if intruderCookie == nil {
		t.Fatalf("第二账号没有拿到 Cookie")
	}

	revoke := coreDo(t, app.Handler(), http.MethodDelete, "/api/apikeys/"+keyID, nil,
		coreCookieHeader(intruderCookie))
	if revoke.status != http.StatusNotFound || coreErrorCode(t, revoke) != "KEY_NOT_FOUND" {
		t.Fatalf("他人不能撤销该 Key: %d %v", revoke.status, revoke.body)
	}

	var revokedAt sql.NullString
	if err := app.db.QueryRow(`SELECT revokedAt FROM "ApiKey" WHERE id = ?`, keyID).Scan(&revokedAt); err != nil {
		t.Fatalf("读取 revokedAt 失败: %v", err)
	}
	if revokedAt.Valid {
		t.Fatalf("他人的撤销请求不得修改 Key")
	}

	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := app.authenticate(w, r, true); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	stillValid := coreDo(t, apiHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if stillValid.status != http.StatusOK {
		t.Fatalf("Key 应当仍然有效: %d %v", stillValid.status, stillValid.body)
	}
}

func TestSessionSecretSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	app1, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("首次 New 失败: %v", err)
	}
	_, cookie := coreRegister(t, app1, "restart@example.com", "secret123")
	keyPath := filepath.Join(dir, authKeyFileName)
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取 auth.key 失败: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat auth.key 失败: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("auth.key 权限应为 0600，实际 %o", mode)
	}
	if err := app1.Close(); err != nil {
		t.Fatalf("关闭 app1 失败: %v", err)
	}

	app2, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("重启 New 失败: %v", err)
	}
	t.Cleanup(func() { _ = app2.Close() })

	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("重启后读取 auth.key 失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("重启不应重新生成 JWT 密钥")
	}

	me := coreDo(t, app2.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie))
	if me.status != http.StatusOK {
		t.Fatalf("重启后旧 Cookie 应继续有效: %d %v", me.status, me.body)
	}
}

func TestLoginRateLimitIgnoresForwardedHeaders(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "limited@example.com", "secret123")

	limited := false
	for i := 0; i < 12; i++ {
		response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
			map[string]string{"email": "limited@example.com", "password": "wrong-password"},
			map[string]string{"X-Forwarded-For": "203.0.113." + string(rune('0'+i))})
		if response.status == http.StatusTooManyRequests {
			if coreErrorCode(t, response) != "RATE_LIMITED" {
				t.Fatalf("限流错误码不正确: %v", response.body)
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("同一 RemoteAddr 的连续登录应当被限流，X-Forwarded-For 不能改变限流键")
	}
}

func TestRequestBodySizeLimit(t *testing.T) {
	app := coreTestApp(t, Config{})
	huge := `{"email":"a@example.com","password":"` + strings.Repeat("x", int(defaultJSONBodyLimit)+1024) + `"}`
	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login", huge, nil)
	if response.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应当 413: %d %v", response.status, response.body)
	}
	if coreErrorCode(t, response) != "REQUEST_TOO_LARGE" {
		t.Fatalf("超大请求体错误码不正确: %v", response.body)
	}
}

func TestRateLimiterIsBounded(t *testing.T) {
	limiter := newRateLimiter(1, time.Minute, 3)
	for i := 0; i < 100; i++ {
		limiter.allow("key-" + hex.EncodeToString([]byte{byte(i)}))
	}
	if size := limiter.size(); size > 3 {
		t.Fatalf("限流 map 必须有界，实际 %d", size)
	}
}
