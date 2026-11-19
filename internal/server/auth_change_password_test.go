package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// ---- helpers for POST /api/auth/change-password（原版 UI 路径） ----

func passwordChangeNew(t *testing.T, app *App, cookie *http.Cookie, payload any, headers map[string]string) *coreResponse {
	t.Helper()
	merged := map[string]string{}
	for name, value := range headers {
		merged[name] = value
	}
	if cookie != nil {
		merged["Cookie"] = cookie.Name + "=" + cookie.Value
	}
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/change-password", payload, merged)
}

func TestChangePasswordKeepsCurrentSessionAndAPIKeys(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "keep@example.com", "secret123")
	otherSession := passwordLoginCookie(t, app, "keep@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")
	if _, err := app.db.Exec(`UPDATE "User" SET mustChangePassword = 1 WHERE email = ?`, "keep@example.com"); err != nil {
		t.Fatalf("设置 mustChangePassword 失败: %v", err)
	}

	response := passwordChangeNew(t, app, cookie,
		map[string]string{"oldPassword": "secret123", "newPassword": "brand-new-secret"}, nil)
	if response.status != http.StatusOK || response.body["ok"] != true {
		t.Fatalf("改密应当成功: %d %v", response.status, response.body)
	}

	// 成功响应必须下发新版本 Cookie（当前会话保持登录），而不是清除 Cookie。
	var freshCookie *string
	for _, candidate := range response.cookies {
		if candidate.Name != app.cfg.AuthCookieName {
			continue
		}
		if candidate.MaxAge < 0 || candidate.Value == "" {
			t.Fatalf("改密成功后不应清除当前 Cookie: %+v", candidate)
		}
		value := candidate.Value
		freshCookie = &value
	}
	if freshCookie == nil {
		t.Fatalf("改密成功后必须重签 Cookie: %v", response.cookies)
	}
	if *freshCookie == cookie.Value {
		t.Fatalf("重签的 Cookie 必须带新的 sessionVersion")
	}

	me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil,
		map[string]string{"Cookie": app.cfg.AuthCookieName + "=" + *freshCookie})
	if me.status != http.StatusOK {
		t.Fatalf("改密后当前会话必须继续可用: %d %v", me.status, me.body)
	}
	if user, _ := me.body["user"].(map[string]any); user["mustChangePassword"] != false {
		t.Fatalf("改密后 mustChangePassword 必须为 false: %v", me.body)
	}

	// 其他设备上的旧会话因 sessionVersion 递增而失效。
	if stale := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(otherSession)); stale.status != http.StatusUnauthorized {
		t.Fatalf("同账号其他会话必须失效: %d %v", stale.status, stale.body)
	}
	if stale := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); stale.status != http.StatusUnauthorized {
		t.Fatalf("旧 Cookie 必须失效: %d %v", stale.status, stale.body)
	}

	// 原版语义：自助改密不撤销 API Key。
	if keyResp := coreDo(t, passwordKeyAuthHandler(app), http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); keyResp.status != http.StatusOK {
		t.Fatalf("自助改密必须保留 API Key: %d %v", keyResp.status, keyResp.body)
	}

	state := passwordLoadState(t, app, "keep@example.com")
	if state.version != 1 {
		t.Fatalf("sessionVersion 必须恰好递增一次: %d", state.version)
	}
	if state.mustChangePwd != 0 {
		t.Fatalf("改密后必须清除 mustChangePassword: %d", state.mustChangePwd)
	}
	if state.activeKeys != 1 {
		t.Fatalf("改密后 API Key 必须保留: %d", state.activeKeys)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(state.hash), []byte("brand-new-secret")); err != nil {
		t.Fatalf("数据库必须保存新密码哈希: %v", err)
	}

	// 新密码可登录，旧密码不可。
	passwordLoginCookie(t, app, "keep@example.com", "brand-new-secret")
	oldLogin := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "keep@example.com", "password": "secret123"}, nil)
	if oldLogin.status != http.StatusBadRequest || coreErrorCode(t, oldLogin) != "INVALID_CREDENTIALS" {
		t.Fatalf("旧密码必须失效: %d %v", oldLogin.status, oldLogin.body)
	}
}

func TestChangePasswordValidationAndNoSideEffects(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "validate@example.com", "secret123")
	before := passwordLoadState(t, app, "validate@example.com")

	cases := map[string]struct {
		payload   string
		status    int
		errorCode string
	}{
		"missing-old":   {`{"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"null-old":      {`{"oldPassword":null,"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"empty-old":     {`{"oldPassword":"","newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"missing-new":   {`{"oldPassword":"secret123"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"null-new":      {`{"oldPassword":"secret123","newPassword":null}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"short-new":     {`{"oldPassword":"secret123","newPassword":"12345"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"number-old":    {`{"oldPassword":123456,"newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_BODY"},
		"non-object":    {`[]`, http.StatusBadRequest, "INVALID_BODY"},
		"wrong-old":     {`{"oldPassword":"not-the-password","newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_CREDENTIALS"},
		"same-password": {`{"oldPassword":"secret123","newPassword":"secret123"}`, http.StatusBadRequest, "SAME_PASSWORD"},
		"too-long-old":  {`{"oldPassword":"` + strings.Repeat("x", maxPasswordBytes+1) + `","newPassword":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_CREDENTIALS"},
		"too-long-new":  {`{"oldPassword":"secret123","newPassword":"` + strings.Repeat("x", maxPasswordBytes+1) + `"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			passwordResetLimiters(app)
			response := passwordChangeNew(t, app, cookie, testCase.payload, nil)
			if response.status != testCase.status {
				t.Fatalf("%s: 期望 %d，实际 %d: %v", name, testCase.status, response.status, response.body)
			}
			if code := coreErrorCode(t, response); code != testCase.errorCode {
				t.Fatalf("%s: 期望错误码 %s，实际 %s", name, testCase.errorCode, code)
			}
			if len(response.cookies) != 0 {
				t.Fatalf("%s: 失败响应不得改写 Cookie: %v", name, response.cookies)
			}
			if after := passwordLoadState(t, app, "validate@example.com"); after != before {
				t.Fatalf("%s: 失败不得有副作用: before=%+v after=%+v", name, before, after)
			}
		})
	}
}

func TestChangePasswordRequiresCookieSession(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "session@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")

	body := map[string]string{"oldPassword": "secret123", "newPassword": "brand-new-secret"}

	bearer := passwordChangeNew(t, app, nil, body, map[string]string{"Authorization": "Bearer " + rawKey})
	if bearer.status != http.StatusUnauthorized {
		t.Fatalf("Bearer 不能改密: %d %v", bearer.status, bearer.body)
	}
	anonymous := passwordChangeNew(t, app, nil, body, nil)
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("无 Cookie 必须 401: %d %v", anonymous.status, anonymous.body)
	}
	if state := passwordLoadState(t, app, "session@example.com"); state.version != 0 {
		t.Fatalf("被拒绝的请求不得改密: %+v", state)
	}
}

func TestChangePasswordConcurrentHasSingleWinner(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "newrace@example.com", "secret123")
	passwordCreateAPIKey(t, app, cookie, "ci")

	bodies := []string{
		`{"oldPassword":"secret123","newPassword":"first-new-secret"}`,
		`{"oldPassword":"secret123","newPassword":"second-new-secret"}`,
	}
	headers := coreCookieHeader(cookie)

	statuses := make([]int, len(bodies))
	bodiesOut := make([]map[string]any, len(bodies))
	var waitGroup sync.WaitGroup
	start := make(chan struct{})
	for index := range bodies {
		waitGroup.Add(1)
		go func(i int) {
			defer waitGroup.Done()
			<-start
			status, parsed := testPasswordDo(app.Handler(), "/api/auth/change-password", bodies[i], headers)
			statuses[i] = status
			bodiesOut[i] = parsed
		}(index)
	}
	close(start)
	waitGroup.Wait()

	winners := 0
	for index, status := range statuses {
		switch status {
		case http.StatusOK:
			winners++
		case http.StatusBadRequest, http.StatusConflict:
			// 输家要么 bcrypt 比较时发现密码已变（400），要么条件更新命中 0 行（409）。
		default:
			t.Fatalf("并发改密出现意外状态 %d: %v", status, bodiesOut[index])
		}
	}
	if winners != 1 {
		t.Fatalf("并发改密只能有一个赢家: %v %v", statuses, bodiesOut)
	}
	state := passwordLoadState(t, app, "newrace@example.com")
	if state.version != 1 {
		t.Fatalf("并发改密后 sessionVersion 应为 1: %d", state.version)
	}
	if state.activeKeys != 1 {
		t.Fatalf("新路径改密后 API Key 必须保留: %d", state.activeKeys)
	}
}
