package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// ---- helpers for the password-change tests (真实临时 SQLite + HTTP) ----

func passwordLoginCookie(t *testing.T, app *App, email, password string) *http.Cookie {
	t.Helper()
	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": email, "password": password}, nil)
	if response.status != http.StatusOK {
		t.Fatalf("登录失败: status=%d body=%v", response.status, response.body)
	}
	for _, cookie := range response.cookies {
		if cookie.Name == app.cfg.AuthCookieName {
			return cookie
		}
	}
	t.Fatalf("登录响应没有 %s Cookie", app.cfg.AuthCookieName)
	return nil
}

func passwordChange(t *testing.T, app *App, cookie *http.Cookie, payload any, headers map[string]string) *coreResponse {
	t.Helper()
	merged := map[string]string{}
	for name, value := range headers {
		merged[name] = value
	}
	if cookie != nil {
		merged["Cookie"] = cookie.Name + "=" + cookie.Value
	}
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/password", payload, merged)
}

// passwordResetLimiters 让同一测试内的多个用例共享 app 但不共享限流桶。
func passwordResetLimiters(app *App) {
	app.passwordLimiter = newRateLimiter(passwordChangeIPLimit, passwordChangeWindow, 64)
	app.passwordUserLimiter = newRateLimiter(passwordChangeUserLimit, passwordChangeWindow, 64)
}

type passwordDBState struct {
	hash          string
	version       int64
	mustChangePwd int
	activeKeys    int
}

func passwordLoadState(t *testing.T, app *App, email string) passwordDBState {
	t.Helper()
	var state passwordDBState
	if err := app.db.QueryRow(
		`SELECT passwordHash, sessionVersion, mustChangePassword FROM "User" WHERE email = ?`, email).
		Scan(&state.hash, &state.version, &state.mustChangePwd); err != nil {
		t.Fatalf("读取账号状态失败: %v", err)
	}
	if err := app.db.QueryRow(
		`SELECT COUNT(*) FROM "ApiKey" k JOIN "User" u ON u.id = k.userId
		 WHERE u.email = ? AND k.revokedAt IS NULL`, email).Scan(&state.activeKeys); err != nil {
		t.Fatalf("读取 API Key 状态失败: %v", err)
	}
	return state
}

func passwordClearCookie(t *testing.T, app *App, response *coreResponse) {
	t.Helper()
	cleared := false
	for _, cookie := range response.cookies {
		if cookie.Name != app.cfg.AuthCookieName {
			continue
		}
		if cookie.MaxAge < 0 && cookie.Value == "" {
			cleared = true
			continue
		}
		t.Fatalf("改密成功后不应签发新会话 Cookie: %+v", cookie)
	}
	if !cleared {
		t.Fatalf("改密成功后必须清除 Cookie: %v", response.cookies)
	}
}

func passwordCreateAPIKey(t *testing.T, app *App, cookie *http.Cookie, name string) string {
	t.Helper()
	created := coreDo(t, app.Handler(), http.MethodPost, "/api/apikeys",
		map[string]string{"name": name}, coreCookieHeader(cookie))
	if created.status != http.StatusOK {
		t.Fatalf("创建 API Key 失败: %d %v", created.status, created.body)
	}
	key, _ := created.body["key"].(map[string]any)
	rawKey, _ := key["rawKey"].(string)
	if rawKey == "" {
		t.Fatalf("创建 API Key 响应缺少 rawKey: %v", created.body)
	}
	return rawKey
}

func passwordKeyAuthHandler(app *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := app.authenticate(w, r, true); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// testPasswordDo 不调用 t.Fatalf，可在并发 goroutine 内安全使用。
func testPasswordDo(handler http.Handler, target, body string, headers map[string]string) (int, map[string]any) {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var parsed map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &parsed)
	return recorder.Code, parsed
}

func TestPasswordChangeInvalidatesSessionsPasswordsAndKeys(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "change@example.com", "secret123")
	secondSession := passwordLoginCookie(t, app, "change@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")

	keyHandler := passwordKeyAuthHandler(app)
	if response := coreDo(t, keyHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); response.status != http.StatusOK {
		t.Fatalf("改密前 Key 应当有效: %d %v", response.status, response.body)
	}

	response := passwordChange(t, app, cookie,
		map[string]string{"currentPassword": "secret123", "newPassword": "brand-new-secret"}, nil)
	if response.status != http.StatusOK || response.body["ok"] != true {
		t.Fatalf("改密应当成功: %d %v", response.status, response.body)
	}
	if message, _ := response.body["message"].(string); !strings.Contains(message, "重新登录") {
		t.Fatalf("成功响应必须提示前端重新登录: %v", response.body)
	}
	passwordClearCookie(t, app, response)

	// 旧 Cookie 与同一账号的另一个会话都因 sessionVersion 递增而失效。
	if stale := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); stale.status != http.StatusUnauthorized {
		t.Fatalf("改密后的旧 Cookie 必须 401: %d %v", stale.status, stale.body)
	}
	if stale := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(secondSession)); stale.status != http.StatusUnauthorized {
		t.Fatalf("同账号其他会话必须 401: %d %v", stale.status, stale.body)
	}

	// 旧 API Key 全部撤销。
	if revoked := coreDo(t, keyHandler, http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); revoked.status != http.StatusUnauthorized ||
		coreErrorCode(t, revoked) != "INVALID_API_KEY" {
		t.Fatalf("改密后旧 API Key 必须失效: %d %v", revoked.status, revoked.body)
	}

	// 旧密码不能登录，新密码可以登录。
	if oldLogin := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "change@example.com", "password": "secret123"}, nil); oldLogin.status != http.StatusBadRequest ||
		coreErrorCode(t, oldLogin) != "INVALID_CREDENTIALS" {
		t.Fatalf("旧密码必须失效: %d %v", oldLogin.status, oldLogin.body)
	}
	newCookie := passwordLoginCookie(t, app, "change@example.com", "brand-new-secret")
	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(newCookie)); me.status != http.StatusOK {
		t.Fatalf("新密码登录后的会话应当有效: %d %v", me.status, me.body)
	}

	state := passwordLoadState(t, app, "change@example.com")
	if state.version != 1 {
		t.Fatalf("sessionVersion 必须恰好递增一次: %d", state.version)
	}
	if state.mustChangePwd != 0 {
		t.Fatalf("改密后 mustChangePassword 必须清零: %d", state.mustChangePwd)
	}
	if state.activeKeys != 0 {
		t.Fatalf("改密后不应剩下未撤销的 API Key: %d", state.activeKeys)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(state.hash), []byte("brand-new-secret")); err != nil {
		t.Fatalf("数据库必须保存新密码的 bcrypt 哈希: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(state.hash), []byte("secret123")); err == nil {
		t.Fatalf("数据库不得仍能匹配旧密码")
	}
}

func TestPasswordChangeWrongCurrentPasswordHasNoSideEffects(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "wrong@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")
	before := passwordLoadState(t, app, "wrong@example.com")

	response := passwordChange(t, app, cookie,
		map[string]string{"currentPassword": "not-the-password", "newPassword": "brand-new-secret"}, nil)
	if response.status != http.StatusBadRequest || coreErrorCode(t, response) != "INVALID_CREDENTIALS" {
		t.Fatalf("错误当前密码必须 400 INVALID_CREDENTIALS: %d %v", response.status, response.body)
	}
	if response.body["ok"] != false {
		t.Fatalf("失败响应必须 ok=false: %v", response.body)
	}
	if len(response.cookies) != 0 {
		t.Fatalf("失败的改密不应改写 Cookie: %v", response.cookies)
	}

	after := passwordLoadState(t, app, "wrong@example.com")
	if after.hash != before.hash || after.version != before.version {
		t.Fatalf("错误密码不得修改密码或 sessionVersion: before=%+v after=%+v", before, after)
	}
	if after.activeKeys != before.activeKeys || after.activeKeys != 1 {
		t.Fatalf("错误密码不得撤销 API Key: before=%d after=%d", before.activeKeys, after.activeKeys)
	}

	// 密码未变：旧 Cookie、旧密码、旧 Key 全部仍然有效。
	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); me.status != http.StatusOK {
		t.Fatalf("失败后原 Cookie 应继续有效: %d %v", me.status, me.body)
	}
	passwordLoginCookie(t, app, "wrong@example.com", "secret123")
	if keyResp := coreDo(t, passwordKeyAuthHandler(app), http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); keyResp.status != http.StatusOK {
		t.Fatalf("失败后原 API Key 应继续有效: %d %v", keyResp.status, keyResp.body)
	}
	if denied := passwordChange(t, app, nil,
		map[string]string{"currentPassword": "secret123", "newPassword": "brand-new-secret"}, nil); denied.status != http.StatusUnauthorized {
		t.Fatalf("错误的尝试也不得成功: %d %v", denied.status, denied.body)
	}
}

func TestPasswordChangePayloadValidation(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "payload@example.com", "secret123")
	if _, err := app.db.Exec(`UPDATE "User" SET mustChangePassword = 1 WHERE email = ?`, "payload@example.com"); err != nil {
		t.Fatalf("设置 mustChangePassword 失败: %v", err)
	}
	before := passwordLoadState(t, app, "payload@example.com")

	longPassword := strings.Repeat("x", maxPasswordBytes+1)
	cases := map[string]struct {
		body      string
		status    int
		errorCode string
	}{
		"missing-currentPassword": {`{"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_PAYLOAD"},
		"null-currentPassword":    {`{"currentPassword":null,"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_PAYLOAD"},
		"missing-newPassword":     {`{"currentPassword":"secret123"}`, http.StatusBadRequest, "INVALID_PAYLOAD"},
		"null-newPassword":        {`{"currentPassword":"secret123","newPassword":null}`, http.StatusBadRequest, "INVALID_PAYLOAD"},
		"empty-object":            {`{}`, http.StatusBadRequest, "INVALID_PAYLOAD"},
		"number-currentPassword":  {`{"currentPassword":123456,"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_BODY"},
		"array-newPassword":       {`{"currentPassword":"secret123","newPassword":["a"]}`, http.StatusBadRequest, "INVALID_BODY"},
		"non-object-body":         {`[]`, http.StatusBadRequest, "INVALID_BODY"},
		"short-newPassword":       {`{"currentPassword":"secret123","newPassword":"12345"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"too-long-newPassword":    {`{"currentPassword":"secret123","newPassword":"` + longPassword + `"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"too-long-currentPass":    {`{"currentPassword":"` + longPassword + `","newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_CREDENTIALS"},
		"empty-currentPassword":   {`{"currentPassword":"","newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_CREDENTIALS"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			passwordResetLimiters(app)
			response := passwordChange(t, app, cookie, testCase.body, nil)
			if response.status != testCase.status {
				t.Fatalf("%s: 期望 %d，实际 %d: %v", name, testCase.status, response.status, response.body)
			}
			if code := coreErrorCode(t, response); code != testCase.errorCode {
				t.Fatalf("%s: 期望错误码 %s，实际 %s", name, testCase.errorCode, code)
			}
			if len(response.cookies) != 0 {
				t.Fatalf("%s: 校验失败不得改写 Cookie: %v", name, response.cookies)
			}
			after := passwordLoadState(t, app, "payload@example.com")
			if after != before {
				t.Fatalf("%s: 校验失败不得有任何副作用: before=%+v after=%+v", name, before, after)
			}
		})
	}

	// 请求体过大时在解码阶段就被拒绝（复用统一 1 MiB 限制）。
	passwordResetLimiters(app)
	huge := `{"currentPassword":"secret123","newPassword":"` + strings.Repeat("y", int(defaultJSONBodyLimit)+1024) + `"}`
	oversized := passwordChange(t, app, cookie, huge, nil)
	if oversized.status != http.StatusRequestEntityTooLarge || coreErrorCode(t, oversized) != "REQUEST_TOO_LARGE" {
		t.Fatalf("超大请求体应当 413: %d %v", oversized.status, oversized.body)
	}
}

func TestPasswordChangeRequiresCookieAndSameOrigin(t *testing.T) {
	app := coreTestApp(t, Config{BaseURL: "https://pic.example.com"})
	_, cookie := coreRegister(t, app, "origin@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")
	before := passwordLoadState(t, app, "origin@example.com")

	body := map[string]string{"currentPassword": "secret123", "newPassword": "brand-new-secret"}

	// API Key（Bearer）不能改密，即使 Key 本身有效。
	bearer := passwordChange(t, app, nil, body, map[string]string{"Authorization": "Bearer " + rawKey})
	if bearer.status != http.StatusUnauthorized || coreErrorCode(t, bearer) != "UNAUTHORIZED" {
		t.Fatalf("Bearer 改密必须 401: %d %v", bearer.status, bearer.body)
	}
	if message, _ := bearer.body["error"].(map[string]any)["message"].(string); !strings.Contains(message, "Cookie") {
		t.Fatalf("Bearer 拒绝原因应说明只接受 Cookie: %v", bearer.body)
	}

	// 未登录（无 Cookie）同样拒绝。
	if anonymous := passwordChange(t, app, nil, body, nil); anonymous.status != http.StatusUnauthorized {
		t.Fatalf("无 Cookie 必须 401: %d %v", anonymous.status, anonymous.body)
	}

	// 跨源写请求在中间件层就被拒绝。
	for _, origin := range []string{"https://evil.example", "null"} {
		crossOrigin := passwordChange(t, app, cookie, body, map[string]string{"Origin": origin})
		if crossOrigin.status != http.StatusForbidden || coreErrorCode(t, crossOrigin) != "CROSS_ORIGIN_DENIED" {
			t.Fatalf("Origin=%s 必须拒绝: %d %v", origin, crossOrigin.status, crossOrigin.body)
		}
	}

	// 配置了 BASEURL 的同源写请求可以到达处理函数（这里用错误密码验证，不改状态）。
	sameOrigin := passwordChange(t, app, cookie,
		map[string]string{"currentPassword": "not-the-password", "newPassword": "brand-new-secret"},
		map[string]string{"Origin": "https://pic.example.com"})
	if sameOrigin.status != http.StatusBadRequest || coreErrorCode(t, sameOrigin) != "INVALID_CREDENTIALS" {
		t.Fatalf("同源请求应进入处理函数: %d %v", sameOrigin.status, sameOrigin.body)
	}

	after := passwordLoadState(t, app, "origin@example.com")
	if after != before {
		t.Fatalf("被拒绝的请求不得有副作用: before=%+v after=%+v", before, after)
	}
}

func TestPasswordChangeRateLimitedIndependently(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "ratelimit@example.com", "secret123")

	limited := false
	for i := 0; i < passwordChangeIPLimit+2; i++ {
		response := passwordChange(t, app, cookie,
			map[string]string{"currentPassword": "not-the-password", "newPassword": "brand-new-secret"}, nil)
		if response.status == http.StatusTooManyRequests {
			if coreErrorCode(t, response) != "RATE_LIMITED" {
				t.Fatalf("限流错误码不正确: %v", response.body)
			}
			limited = true
			break
		}
		if response.status != http.StatusBadRequest {
			t.Fatalf("限流前的错误密码应当 400: %d %v", response.status, response.body)
		}
	}
	if !limited {
		t.Fatalf("同一 IP 的连续改密尝试必须被独立限流")
	}
	if state := passwordLoadState(t, app, "ratelimit@example.com"); state.version != 0 || state.activeKeys != 0 {
		t.Fatalf("限流不会改动账号状态: %+v", state)
	}
}

func TestPasswordChangeConditionalUpdateRejectsStaleRecord(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "stale@example.com", "secret123")

	// Cookie 只用于确认账号可登录；这里直接驱动事务，确定性覆盖并发互斥条件。
	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); me.status != http.StatusOK {
		t.Fatalf("账号应当可登录: %d %v", me.status, me.body)
	}
	record, err := app.loadUserByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	firstHash, err := bcrypt.GenerateFromPassword([]byte("first-new-secret"), bcryptCost)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	secondHash, err := bcrypt.GenerateFromPassword([]byte("second-new-secret"), bcryptCost)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/password", nil)

	if err := app.applyPasswordChange(request, record, firstHash); err != nil {
		t.Fatalf("首次改密应当成功: %v", err)
	}
	// 用同一份旧记录再改一次（模拟并发旧会话）必须被条件更新拒绝。
	if err := app.applyPasswordChange(request, record, secondHash); err != errPasswordStale {
		t.Fatalf("过期记录必须被拒绝: %v", err)
	}

	var stored string
	var version int64
	if err := app.db.QueryRow(`SELECT passwordHash, sessionVersion FROM "User" WHERE id = 1`).
		Scan(&stored, &version); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if stored != string(firstHash) {
		t.Fatalf("旧会话不得覆盖已写入的新密码")
	}
	if version != 1 {
		t.Fatalf("sessionVersion 只应递增一次: %d", version)
	}
}

func TestPasswordChangeConcurrentHasSingleWinner(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "race@example.com", "secret123")
	passwordCreateAPIKey(t, app, cookie, "ci")
	if _, err := app.db.Exec(`UPDATE "User" SET mustChangePassword = 1 WHERE email = ?`, "race@example.com"); err != nil {
		t.Fatalf("设置 mustChangePassword 失败: %v", err)
	}

	newPasswords := []string{"first-new-secret", "second-new-secret"}
	bodies := []string{
		`{"currentPassword":"secret123","newPassword":"first-new-secret"}`,
		`{"currentPassword":"secret123","newPassword":"second-new-secret"}`,
	}
	headers := coreCookieHeader(cookie)

	const attempts = 2
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		statuses  = make([]int, attempts)
		bodiesOut = make([]map[string]any, attempts)
		start     = make(chan struct{})
	)
	for i := 0; i < attempts; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			status, parsed := testPasswordDo(app.Handler(), "/api/auth/password", bodies[index], headers)
			mu.Lock()
			statuses[index] = status
			bodiesOut[index] = parsed
			mu.Unlock()
		}(i)
	}
	close(start)
	waitGroup.Wait()

	winner := -1
	for index, status := range statuses {
		switch status {
		case http.StatusOK:
			if winner != -1 {
				t.Fatalf("并发改密只能有一个赢家: %v %v", statuses, bodiesOut)
			}
			winner = index
		case http.StatusBadRequest, http.StatusConflict:
			// 输家要么在 bcrypt 比较时发现密码已变（400），要么条件更新命中 0 行（409）。
		default:
			t.Fatalf("并发改密出现意外状态 %d: %v", status, bodiesOut[index])
		}
	}
	if winner == -1 {
		t.Fatalf("并发改密应当恰好有一个成功: %v %v", statuses, bodiesOut)
	}

	state := passwordLoadState(t, app, "race@example.com")
	if state.version != 1 {
		t.Fatalf("并发改密后 sessionVersion 应为 1: %d", state.version)
	}
	if state.activeKeys != 0 {
		t.Fatalf("赢家事务必须撤销全部 API Key: %d", state.activeKeys)
	}
	if state.mustChangePwd != 0 {
		t.Fatalf("赢家事务必须清除 mustChangePassword: %d", state.mustChangePwd)
	}

	// 数据库里只保存赢家的新密码，输家的密码从未生效。
	loser := 1 - winner
	if err := bcrypt.CompareHashAndPassword([]byte(state.hash), []byte(newPasswords[winner])); err != nil {
		t.Fatalf("赢家的新密码应当可验证: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(state.hash), []byte(newPasswords[loser])); err == nil {
		t.Fatalf("输家的新密码不得写入数据库")
	}

	// 只有赢家的密码能登录；旧密码与输家密码都不能。
	passwordLoginCookie(t, app, "race@example.com", newPasswords[winner])
	for _, rejected := range []string{"secret123", newPasswords[loser]} {
		login := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
			map[string]string{"email": "race@example.com", "password": rejected}, nil)
		if login.status != http.StatusBadRequest || coreErrorCode(t, login) != "INVALID_CREDENTIALS" {
			t.Fatalf("密码 %q 不得能登录: %d %v", rejected, login.status, login.body)
		}
	}
}
