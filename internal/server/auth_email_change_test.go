package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- helpers for change-email / change-email confirm ----

func coreChangeEmail(t *testing.T, app *App, cookie *http.Cookie, email, password string) *coreResponse {
	t.Helper()
	headers := map[string]string{}
	if cookie != nil {
		headers["Cookie"] = cookie.Name + "=" + cookie.Value
	}
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/change-email",
		map[string]any{"email": email, "password": password}, headers)
}

func coreConfirmChangeEmail(t *testing.T, app *App, cookie *http.Cookie, token string) *coreResponse {
	t.Helper()
	headers := map[string]string{}
	if cookie != nil {
		headers["Cookie"] = cookie.Name + "=" + cookie.Value
	}
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/change-email/confirm",
		map[string]any{"token": token}, headers)
}

func TestChangeEmailValidationWithoutMail(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "emailchange@example.com", "secret123")

	invalid := coreChangeEmail(t, app, cookie, "not-an-email", "secret123")
	if invalid.status != http.StatusBadRequest || coreErrorCode(t, invalid) != "INVALID_EMAIL" {
		t.Fatalf("非法邮箱应当 400 INVALID_EMAIL: %d %v", invalid.status, invalid.body)
	}

	wrongPassword := coreChangeEmail(t, app, cookie, "newemail@example.com", "wrong-password")
	if wrongPassword.status != http.StatusBadRequest || coreErrorCode(t, wrongPassword) != "INVALID_CREDENTIALS" {
		t.Fatalf("错误密码应当 400 INVALID_CREDENTIALS: %d %v", wrongPassword.status, wrongPassword.body)
	}

	same := coreChangeEmail(t, app, cookie, "emailchange@example.com", "secret123")
	if same.status != http.StatusBadRequest || coreErrorCode(t, same) != "SAME_EMAIL" {
		t.Fatalf("相同邮箱应当 400 SAME_EMAIL: %d %v", same.status, same.body)
	}

	unavailable := coreChangeEmail(t, app, cookie, "newemail@example.com", "secret123")
	if unavailable.status != http.StatusServiceUnavailable || coreErrorCode(t, unavailable) != "MAIL_UNAVAILABLE" {
		t.Fatalf("未配置邮件应当 503 MAIL_UNAVAILABLE: %d %v", unavailable.status, unavailable.body)
	}

	anonymous := coreChangeEmail(t, app, nil, "newemail@example.com", "secret123")
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("改邮箱必须要求登录 Cookie: %d %v", anonymous.status, anonymous.body)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("失败的请求不得签发令牌: %d", count)
	}
}

func TestChangeEmailRejectsTakenAddressAndSendsToken(t *testing.T) {
	app, capture := coreMailApp(t)
	_, cookie := coreRegister(t, app, "owner@example.com", "secret123")
	coreInsertUser(t, app, "taken@example.com", "secret123", roleUser)

	taken := coreChangeEmail(t, app, cookie, "taken@example.com", "secret123")
	if taken.status != http.StatusConflict || coreErrorCode(t, taken) != "EMAIL_EXISTS" {
		t.Fatalf("已占用邮箱应当 409 EMAIL_EXISTS: %d %v", taken.status, taken.body)
	}
	if capture.count() != 0 {
		t.Fatalf("冲突时不应发送邮件: %d", capture.count())
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("冲突时不应签发令牌: %d", count)
	}

	ok := coreChangeEmail(t, app, cookie, "owner-new@example.com", "secret123")
	if ok.status != http.StatusOK || ok.body["ok"] != true {
		t.Fatalf("有效改邮箱请求应当成功: %d %v", ok.status, ok.body)
	}
	msg := capture.wait(t, 0)
	if msg.To != "owner-new@example.com" {
		t.Fatalf("确认邮件应发到新邮箱: %q", msg.To)
	}
	token := coreTokenFromMail(t, msg)
	if !strings.Contains(msg.Text, "https://pic.example.com/verify-email#token="+token) {
		t.Fatalf("确认邮件应包含 verify-email 链接: %s", msg.Text)
	}

	var (
		purpose      string
		targetEmail  string
		currentEmail string
		version      int64
	)
	if err := app.db.QueryRow(
		`SELECT purpose, email, currentEmail, sessionVersion FROM "AccountToken" WHERE userId = 1`).
		Scan(&purpose, &targetEmail, &currentEmail, &version); err != nil {
		t.Fatalf("读取账号令牌失败: %v", err)
	}
	if purpose != accountTokenPurposeEmailChange || targetEmail != "owner-new@example.com" ||
		currentEmail != "owner@example.com" || version != 0 {
		t.Fatalf("令牌记录不符合契约: purpose=%s email=%s currentEmail=%s version=%d",
			purpose, targetEmail, currentEmail, version)
	}
}

func TestChangeEmailConfirmSwitchesAddressAndSessions(t *testing.T) {
	app, capture := coreMailApp(t)
	_, cookie := coreRegister(t, app, "confirm@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")

	if response := coreChangeEmail(t, app, cookie, "confirm-new@example.com", "secret123"); response.status != http.StatusOK {
		t.Fatalf("申请改邮箱失败: %d %v", response.status, response.body)
	}
	token := coreTokenFromMail(t, capture.wait(t, 0))

	response := coreConfirmChangeEmail(t, app, cookie, token)
	if response.status != http.StatusOK || response.body["ok"] != true {
		t.Fatalf("确认改邮箱应当成功: %d %v", response.status, response.body)
	}
	if response.body["email"] != "confirm-new@example.com" {
		t.Fatalf("成功响应应返回新邮箱: %v", response.body)
	}
	cleared := false
	for _, candidate := range response.cookies {
		if candidate.Name == app.cfg.AuthCookieName && candidate.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("改邮箱后必须清除当前 Cookie: %v", response.cookies)
	}

	var (
		email   string
		version int64
	)
	if err := app.db.QueryRow(`SELECT email, sessionVersion FROM "User" WHERE id = 1`).Scan(&email, &version); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if email != "confirm-new@example.com" || version != 1 {
		t.Fatalf("改邮箱必须写入新地址并递增 sessionVersion: email=%s version=%d", email, version)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("确认后必须清空该账号令牌: %d", count)
	}
	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); me.status != http.StatusUnauthorized {
		t.Fatalf("改邮箱后旧会话必须失效: %d %v", me.status, me.body)
	}
	// 原版语义：改邮箱不撤销 API Key（只有重置密码与管理员重置才撤销）。
	if keyResp := coreDo(t, passwordKeyAuthHandler(app), http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); keyResp.status != http.StatusOK {
		t.Fatalf("改邮箱不应撤销 API Key: %d %v", keyResp.status, keyResp.body)
	}

	// 改邮箱后旧 Cookie 已失效；用新邮箱重新登录后再用同一令牌必须失败。
	newCookie := passwordLoginCookie(t, app, "confirm-new@example.com", "secret123")
	reuse := coreConfirmChangeEmail(t, app, newCookie, token)
	if reuse.status != http.StatusBadRequest || coreErrorCode(t, reuse) != "INVALID_TOKEN" {
		t.Fatalf("确认令牌只能使用一次: %d %v", reuse.status, reuse.body)
	}
	// 旧 Cookie 在改邮箱后就已失效。
	if stale := coreConfirmChangeEmail(t, app, cookie, token); stale.status != http.StatusUnauthorized {
		t.Fatalf("旧会话必须失效: %d %v", stale.status, stale.body)
	}
}

func TestChangeEmailConfirmRejectsInvalidTokens(t *testing.T) {
	app, capture := coreMailApp(t)
	_, cookie := coreRegister(t, app, "badconfirm@example.com", "secret123")
	coreInsertUser(t, app, "other@example.com", "secret123", roleUser)

	if response := coreChangeEmail(t, app, cookie, "badconfirm-new@example.com", "secret123"); response.status != http.StatusOK {
		t.Fatalf("申请改邮箱失败: %d %v", response.status, response.body)
	}
	validToken := coreTokenFromMail(t, capture.wait(t, 0))

	before := passwordLoadState(t, app, "badconfirm@example.com")
	cases := map[string]struct {
		token     string
		status    int
		errorCode string
	}{
		"missing":   {"", http.StatusBadRequest, "INVALID_TOKEN"},
		"malformed": {"not-a-token", http.StatusBadRequest, "INVALID_TOKEN"},
		"unknown":   {strings.Repeat("d", 64), http.StatusBadRequest, "INVALID_TOKEN"},
		"uppercase": {strings.ToUpper(validToken), http.StatusBadRequest, "INVALID_TOKEN"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			response := coreConfirmChangeEmail(t, app, cookie, testCase.token)
			if response.status != testCase.status || coreErrorCode(t, response) != testCase.errorCode {
				t.Fatalf("%s: %d %v", name, response.status, response.body)
			}
			if after := passwordLoadState(t, app, "badconfirm@example.com"); after != before {
				t.Fatalf("%s: 失败不得有副作用: before=%+v after=%+v", name, before, after)
			}
		})
	}

	// 未登录不能确认。
	if anonymous := coreConfirmChangeEmail(t, app, nil, validToken); anonymous.status != http.StatusUnauthorized {
		t.Fatalf("确认改邮箱必须要求登录: %d %v", anonymous.status, anonymous.body)
	}

	// 令牌属于别的用途（找回密码）时不能用于改邮箱。
	purposeToken := strings.Repeat("e", 64)
	coreInsertAccountToken(t, app, 1, accountTokenPurposePasswordReset, purposeToken,
		"badconfirm-new@example.com", "badconfirm@example.com", 0, coreTimestamp(time.Now().Add(time.Minute)))
	wrongPurpose := coreConfirmChangeEmail(t, app, cookie, purposeToken)
	if wrongPurpose.status != http.StatusBadRequest || coreErrorCode(t, wrongPurpose) != "INVALID_TOKEN" {
		t.Fatalf("用途不匹配的令牌必须被拒绝: %d %v", wrongPurpose.status, wrongPurpose.body)
	}

	// sessionVersion 变化（例如期间改过密码）后旧确认链接失效。
	if _, err := app.db.Exec(`UPDATE "User" SET sessionVersion = sessionVersion + 1 WHERE id = 1`); err != nil {
		t.Fatalf("递增 sessionVersion 失败: %v", err)
	}
	stale := coreConfirmChangeEmail(t, app, cookie, validToken)
	if stale.status != http.StatusUnauthorized {
		t.Fatalf("旧会话应先被会话校验拒绝: %d %v", stale.status, stale.body)
	}
	// 用新会话再试：令牌的 sessionVersion 已过期。
	freshCookie := passwordLoginCookie(t, app, "badconfirm@example.com", "secret123")
	expired := coreConfirmChangeEmail(t, app, freshCookie, validToken)
	if expired.status != http.StatusBadRequest || coreErrorCode(t, expired) != "INVALID_TOKEN" {
		t.Fatalf("sessionVersion 不匹配的令牌必须被拒绝: %d %v", expired.status, expired.body)
	}

	// 过期令牌。
	expiredRaw := strings.Repeat("f", 64)
	coreInsertAccountToken(t, app, 1, accountTokenPurposeEmailChange, expiredRaw,
		"late@example.com", "badconfirm@example.com", 1, coreTimestamp(time.Now().Add(-time.Minute)))
	expiredToken := coreConfirmChangeEmail(t, app, freshCookie, expiredRaw)
	if expiredToken.status != http.StatusBadRequest || coreErrorCode(t, expiredToken) != "INVALID_TOKEN" {
		t.Fatalf("过期令牌必须被拒绝: %d %v", expiredToken.status, expiredToken.body)
	}

	// 直到这里邮箱都没有被改动。
	var email string
	if err := app.db.QueryRow(`SELECT email FROM "User" WHERE id = 1`).Scan(&email); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if email != "badconfirm@example.com" {
		t.Fatalf("失败的确认不得改动邮箱: %s", email)
	}
}

func TestChangeEmailConfirmConflictWithExistingAccount(t *testing.T) {
	app, _ := coreMailApp(t)
	_, cookie := coreRegister(t, app, "first@example.com", "secret123")

	rawToken := strings.Repeat("a1", 32)
	coreInsertAccountToken(t, app, 1, accountTokenPurposeEmailChange, rawToken,
		"later-taken@example.com", "first@example.com", 0, coreTimestamp(time.Now().Add(time.Minute)))
	// 在确认之前该地址被其他账号占用。
	coreInsertUser(t, app, "later-taken@example.com", "secret123", roleUser)

	response := coreConfirmChangeEmail(t, app, cookie, rawToken)
	if response.status != http.StatusConflict || coreErrorCode(t, response) != "EMAIL_EXISTS" {
		t.Fatalf("邮箱冲突应当 409 EMAIL_EXISTS: %d %v", response.status, response.body)
	}

	var (
		email   string
		version int64
	)
	if err := app.db.QueryRow(`SELECT email, sessionVersion FROM "User" WHERE id = 1`).Scan(&email, &version); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if email != "first@example.com" || version != 0 {
		t.Fatalf("冲突时必须回滚: email=%s version=%d", email, version)
	}
	// 冲突后令牌保留，用户可以改回旧地址或让管理员处理。
	if count := coreAccountTokenCount(t, app, 1); count != 1 {
		t.Fatalf("冲突回滚后令牌应保留: %d", count)
	}
}

func TestChangeEmailDeliveryFailureReturns503(t *testing.T) {
	app, capture := coreMailApp(t)
	_, cookie := coreRegister(t, app, "failmail@example.com", "secret123")
	capture.failWith(fmt.Errorf("smtp 连接失败"))

	response := coreChangeEmail(t, app, cookie, "failmail-new@example.com", "secret123")
	if response.status != http.StatusServiceUnavailable || coreErrorCode(t, response) != "MAIL_UNAVAILABLE" {
		t.Fatalf("投递失败应当 503 MAIL_UNAVAILABLE: %d %v", response.status, response.body)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("投递失败后不得留下令牌: %d", count)
	}
	var email string
	if err := app.db.QueryRow(`SELECT email FROM "User" WHERE id = 1`).Scan(&email); err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if email != "failmail@example.com" {
		t.Fatalf("投递失败不得改动邮箱: %s", email)
	}
}
