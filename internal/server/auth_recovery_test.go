package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers for forgot-password / reset-password / change-email ----

// coreMailCapture 是注入的投递实现：记录邮件内容，可模拟投递失败。
type coreMailCapture struct {
	mu       sync.Mutex
	messages []mailMessage
	failures int
	err      error
}

func (c *coreMailCapture) send(_ context.Context, msg mailMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		c.failures++
		return c.err
	}
	c.messages = append(c.messages, msg)
	return nil
}

func (c *coreMailCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

func (c *coreMailCapture) failWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// wait 轮询等待第 index 封邮件（后台投递没有同步回调，只能轮询）。
func (c *coreMailCapture) wait(t *testing.T, index int) mailMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.messages) > index {
			msg := c.messages[index]
			c.mu.Unlock()
			return msg
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 封邮件超时", index+1)
	return mailMessage{}
}

// coreMailApp 创建配好 SMTP 与注入投递的应用：不发真实邮件。
func coreMailApp(t *testing.T) (*App, *coreMailCapture) {
	t.Helper()
	app := coreTestApp(t, Config{
		BaseURL: "https://pic.example.com",
		SMTP: SMTPConfig{
			Host:       "smtp.example.com",
			Port:       587,
			RequireTLS: true,
			From:       "noreply@example.com",
		},
	})
	capture := &coreMailCapture{}
	app.mailSend = capture.send
	return app, capture
}

// coreTokenFromMail 从邮件正文里取出一次性令牌（放在 URL fragment 中）。
func coreTokenFromMail(t *testing.T, msg mailMessage) string {
	t.Helper()
	marker := "#token="
	index := strings.Index(msg.Text, marker)
	if index < 0 {
		t.Fatalf("邮件正文缺少令牌链接: %s", msg.Text)
	}
	rest := msg.Text[index+len(marker):]
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

func coreRequestRecoveryToken(t *testing.T, app *App, capture *coreMailCapture, email string) string {
	t.Helper()
	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": email}, nil)
	if response.status != http.StatusOK {
		t.Fatalf("找回密码应当恒 200: %d %v", response.status, response.body)
	}
	return coreTokenFromMail(t, capture.wait(t, capture.count()))
}

func coreInsertAccountToken(t *testing.T, app *App, userID int64, purpose, rawToken, email, currentEmail string, sessionVersion int64, expiresAt string) {
	t.Helper()
	// 同一账号每种用途至多一条：用 upsert 覆盖，模拟“重新申请后旧链接失效”。
	if _, err := app.db.Exec(
		`INSERT INTO "AccountToken" (id, userId, purpose, tokenHash, email, currentEmail, sessionVersion, expiresAt, createdAt)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(userId, purpose) DO UPDATE SET
		   id = excluded.id, tokenHash = excluded.tokenHash, email = excluded.email,
		   currentEmail = excluded.currentEmail, sessionVersion = excluded.sessionVersion,
		   expiresAt = excluded.expiresAt, createdAt = excluded.createdAt`,
		fmt.Sprintf("token_%s", hashAccountTokenHex(rawToken)), userID, purpose, hashAccountTokenHex(rawToken),
		email, currentEmail, sessionVersion, expiresAt, coreNow()); err != nil {
		t.Fatalf("插入账号令牌失败: %v", err)
	}
}

func coreResetPassword(t *testing.T, app *App, token, password string) *coreResponse {
	t.Helper()
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/reset-password",
		map[string]any{"token": token, "password": password}, nil)
}

func coreAccountTokenCount(t *testing.T, app *App, userID int64) int {
	t.Helper()
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM "AccountToken" WHERE userId = ?`, userID).Scan(&count); err != nil {
		t.Fatalf("统计账号令牌失败: %v", err)
	}
	return count
}

func TestForgotPasswordRequiresConfiguredMail(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "nomail@example.com", "secret123")

	invalid := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "not-an-email"}, nil)
	if invalid.status != http.StatusBadRequest || coreErrorCode(t, invalid) != "INVALID_EMAIL" {
		t.Fatalf("非法邮箱应当 400 INVALID_EMAIL: %d %v", invalid.status, invalid.body)
	}

	unavailable := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "nomail@example.com"}, nil)
	if unavailable.status != http.StatusServiceUnavailable || coreErrorCode(t, unavailable) != "MAIL_UNAVAILABLE" {
		t.Fatalf("未配置邮件应当 503 MAIL_UNAVAILABLE: %d %v", unavailable.status, unavailable.body)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("未配置邮件时不应签发令牌: %d", count)
	}
}

func TestForgotPasswordDeliversTokenForKnownEmailOnly(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "known@example.com", "secret123")

	token := coreRequestRecoveryToken(t, app, capture, "known@example.com")
	if len(token) != 64 || !accountTokenPattern.MatchString(token) {
		t.Fatalf("令牌应当是 64 位小写 hex: %q", token)
	}
	msg := capture.wait(t, 0)
	if msg.To != "known@example.com" {
		t.Fatalf("邮件收件人应为请求邮箱: %q", msg.To)
	}
	if !strings.Contains(msg.Text, "https://pic.example.com/reset-password#token="+token) {
		t.Fatalf("邮件应包含绝对重置链接: %s", msg.Text)
	}

	// 数据库只保存哈希，不保存令牌明文。
	var stored string
	if err := app.db.QueryRow(`SELECT tokenHash FROM "AccountToken" WHERE purpose = ?`,
		accountTokenPurposePasswordReset).Scan(&stored); err != nil {
		t.Fatalf("读取令牌哈希失败: %v", err)
	}
	if stored == token || stored != hashAccountTokenHex(token) {
		t.Fatalf("数据库只能保存令牌的 sha256 哈希")
	}

	// 未知邮箱同样返回 200，且不投递、不写令牌。
	unknown := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "missing@example.com"}, nil)
	if unknown.status != http.StatusOK || unknown.body["ok"] != true {
		t.Fatalf("未知邮箱也必须返回 ok: %d %v", unknown.status, unknown.body)
	}
	time.Sleep(200 * time.Millisecond)
	if count := capture.count(); count != 1 {
		t.Fatalf("未知邮箱不应投递邮件: %d", count)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 1 {
		t.Fatalf("未知邮箱不应新增令牌: %d", count)
	}

	// 已禁用账号同样静默：不投递新邮件。
	if _, err := app.db.Exec(`UPDATE "User" SET disabledAt = ? WHERE email = ?`, coreNow(), "known@example.com"); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}
	disabled := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "known@example.com"}, nil)
	if disabled.status != http.StatusOK {
		t.Fatalf("禁用账号也必须返回 ok: %d %v", disabled.status, disabled.body)
	}
	time.Sleep(200 * time.Millisecond)
	if count := capture.count(); count != 1 {
		t.Fatalf("禁用账号不应投递邮件: %d", count)
	}
}

func TestForgotPasswordDeliveryFailureKeepsTokenTableClean(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "delivery@example.com", "secret123")
	capture.failWith(fmt.Errorf("smtp 连接失败"))

	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "delivery@example.com"}, nil)
	if response.status != http.StatusOK {
		t.Fatalf("投递失败也不能暴露给公开接口: %d %v", response.status, response.body)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		failures := capture.failures
		capture.mu.Unlock()
		if failures > 0 {
			// 投递失败的那条令牌必须按 tokenHash 删除，不能留下无法使用的记录。
			if count := coreAccountTokenCount(t, app, 1); count != 0 {
				t.Fatalf("投递失败后不应留下令牌: %d", count)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("应当尝试投递并失败一次")
}

func TestResetPasswordSuccessRevokesSessionsAndKeys(t *testing.T) {
	app, capture := coreMailApp(t)
	_, cookie := coreRegister(t, app, "reset@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, cookie, "ci")
	token := coreRequestRecoveryToken(t, app, capture, "reset@example.com")

	response := coreResetPassword(t, app, token, "brand-new-secret")
	if response.status != http.StatusOK || response.body["ok"] != true {
		t.Fatalf("重置密码应当成功: %d %v", response.status, response.body)
	}
	cleared := false
	for _, candidate := range response.cookies {
		if candidate.Name == app.cfg.AuthCookieName && candidate.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("重置密码后必须清除 Cookie: %v", response.cookies)
	}

	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(cookie)); me.status != http.StatusUnauthorized {
		t.Fatalf("重置后旧会话必须失效: %d %v", me.status, me.body)
	}
	if keyResp := coreDo(t, passwordKeyAuthHandler(app), http.MethodGet, "/api/images", nil,
		map[string]string{"Authorization": "Bearer " + rawKey}); keyResp.status != http.StatusUnauthorized {
		t.Fatalf("重置后 API Key 必须失效: %d %v", keyResp.status, keyResp.body)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 0 {
		t.Fatalf("重置成功后必须清空该账号令牌: %d", count)
	}

	state := passwordLoadState(t, app, "reset@example.com")
	if state.version != 1 {
		t.Fatalf("重置密码应恰好递增一次 sessionVersion: %d", state.version)
	}
	if state.mustChangePwd != 0 {
		t.Fatalf("重置密码应清除 mustChangePassword: %d", state.mustChangePwd)
	}
	passwordLoginCookie(t, app, "reset@example.com", "brand-new-secret")

	// 同一令牌再次使用必须失败。
	reuse := coreResetPassword(t, app, token, "third-new-secret")
	if reuse.status != http.StatusBadRequest || coreErrorCode(t, reuse) != "INVALID_TOKEN" {
		t.Fatalf("令牌只能使用一次: %d %v", reuse.status, reuse.body)
	}
}

func TestResetPasswordRejectsInvalidTokens(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "invalid@example.com", "secret123")
	validToken := coreRequestRecoveryToken(t, app, capture, "invalid@example.com")
	before := passwordLoadState(t, app, "invalid@example.com")

	cases := map[string]struct {
		body      string
		status    int
		errorCode string
	}{
		"missing-token": {`{"password":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_TOKEN"},
		"null-token":    {`{"token":null,"password":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_TOKEN"},
		"empty-token":   {`{"token":"","password":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_TOKEN"},
		"short-token":   {`{"token":"abc","password":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_TOKEN"},
		"non-hex-token": {`{"token":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz","password":"brand-new-secret"}`, http.StatusBadRequest, "INVALID_TOKEN"},
		"uppercase-token": {`{"token":"` + strings.ToUpper(validToken) + `","password":"brand-new-secret"}`,
			http.StatusBadRequest, "INVALID_TOKEN"},
		"unknown-token": {`{"token":"` + strings.Repeat("a", 64) + `","password":"brand-new-secret"}`,
			http.StatusBadRequest, "INVALID_TOKEN"},
		"short-password": {`{"token":"` + validToken + `","password":"12345"}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"null-password":  {`{"token":"` + validToken + `","password":null}`, http.StatusBadRequest, "INVALID_PASSWORD"},
		"non-object":     {`[]`, http.StatusBadRequest, "INVALID_BODY"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/reset-password", testCase.body, nil)
			if response.status != testCase.status {
				t.Fatalf("%s: 期望 %d，实际 %d: %v", name, testCase.status, response.status, response.body)
			}
			if code := coreErrorCode(t, response); code != testCase.errorCode {
				t.Fatalf("%s: 期望错误码 %s，实际 %s", name, testCase.errorCode, code)
			}
			if len(response.cookies) != 0 {
				t.Fatalf("%s: 失败响应不得改写 Cookie: %v", name, response.cookies)
			}
			if after := passwordLoadState(t, app, "invalid@example.com"); after != before {
				t.Fatalf("%s: 失败不得有副作用: before=%+v after=%+v", name, before, after)
			}
		})
	}

	// 合法的令牌在整个失败用例集之后仍然可用（失败用例不消费令牌）。
	if response := coreResetPassword(t, app, validToken, "brand-new-secret"); response.status != http.StatusOK {
		t.Fatalf("合法令牌应当仍然可用: %d %v", response.status, response.body)
	}
}

func TestResetPasswordRejectsExpiredTokenAndStaleState(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "stale@example.com", "secret123")

	// 过期令牌。
	raw := strings.Repeat("b", 64)
	past := coreTimestamp(time.Now().Add(-time.Minute))
	coreInsertAccountToken(t, app, 1, accountTokenPurposePasswordReset, raw, "stale@example.com", "stale@example.com", 0, past)
	expired := coreResetPassword(t, app, raw, "brand-new-secret")
	if expired.status != http.StatusBadRequest || coreErrorCode(t, expired) != "INVALID_TOKEN" {
		t.Fatalf("过期令牌必须被拒绝: %d %v", expired.status, expired.body)
	}

	// sessionVersion 与签发时不一致（例如期间改过密码）。
	serviceToken := coreRequestRecoveryToken(t, app, capture, "stale@example.com")
	if _, err := app.db.Exec(`UPDATE "User" SET sessionVersion = 5 WHERE id = 1`); err != nil {
		t.Fatalf("递增 sessionVersion 失败: %v", err)
	}
	stale := coreResetPassword(t, app, serviceToken, "brand-new-secret")
	if stale.status != http.StatusBadRequest || coreErrorCode(t, stale) != "INVALID_TOKEN" {
		t.Fatalf("sessionVersion 变化的令牌必须被拒绝: %d %v", stale.status, stale.body)
	}

	// 邮箱与签发时不一致。
	if _, err := app.db.Exec(`UPDATE "User" SET email = ?, sessionVersion = 0 WHERE id = 1`, "moved@example.com"); err != nil {
		t.Fatalf("改邮箱失败: %v", err)
	}
	movedToken := coreRequestRecoveryToken(t, app, capture, "moved@example.com")
	if _, err := app.db.Exec(`UPDATE "User" SET email = ? WHERE id = 1`, "changed-again@example.com"); err != nil {
		t.Fatalf("改邮箱失败: %v", err)
	}
	moved := coreResetPassword(t, app, movedToken, "brand-new-secret")
	if moved.status != http.StatusBadRequest || coreErrorCode(t, moved) != "INVALID_TOKEN" {
		t.Fatalf("邮箱变化的令牌必须被拒绝: %d %v", moved.status, moved.body)
	}

	// 账号被禁用。
	var currentEmail string
	var currentVersion int64
	if err := app.db.QueryRow(`SELECT email, sessionVersion FROM "User" WHERE id = 1`).
		Scan(&currentEmail, &currentVersion); err != nil {
		t.Fatalf("读取账号状态失败: %v", err)
	}
	disabledRaw := strings.Repeat("c", 64)
	coreInsertAccountToken(t, app, 1, accountTokenPurposePasswordReset, disabledRaw,
		currentEmail, currentEmail, currentVersion, coreTimestamp(time.Now().Add(time.Minute)))
	if _, err := app.db.Exec(`UPDATE "User" SET disabledAt = ? WHERE id = 1`, coreNow()); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}
	disabled := coreResetPassword(t, app, disabledRaw, "brand-new-secret")
	if disabled.status != http.StatusBadRequest || coreErrorCode(t, disabled) != "INVALID_TOKEN" {
		t.Fatalf("禁用账号的令牌必须被拒绝: %d %v", disabled.status, disabled.body)
	}
}

func TestResetPasswordRejectsSamePasswordWithoutConsumingToken(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "same@example.com", "secret123")
	token := coreRequestRecoveryToken(t, app, capture, "same@example.com")

	same := coreResetPassword(t, app, token, "secret123")
	if same.status != http.StatusBadRequest || coreErrorCode(t, same) != "SAME_PASSWORD" {
		t.Fatalf("新密码不能与原密码相同: %d %v", same.status, same.body)
	}
	if count := coreAccountTokenCount(t, app, 1); count != 1 {
		t.Fatalf("SAME_PASSWORD 不应消费令牌: %d", count)
	}
	if response := coreResetPassword(t, app, token, "brand-new-secret"); response.status != http.StatusOK {
		t.Fatalf("同一令牌之后仍应可用: %d %v", response.status, response.body)
	}
}

func TestResetPasswordConcurrentConsumptionHasSingleWinner(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "resrace@example.com", "secret123")
	token := coreRequestRecoveryToken(t, app, capture, "resrace@example.com")

	passwords := []string{"first-new-secret", "second-new-secret"}
	statuses := make([]int, len(passwords))
	var waitGroup sync.WaitGroup
	start := make(chan struct{})
	for index := range passwords {
		waitGroup.Add(1)
		go func(i int) {
			defer waitGroup.Done()
			<-start
			status, _ := testPasswordDo(app.Handler(), "/api/auth/reset-password",
				`{"token":"`+token+`","password":"`+passwords[i]+`"}`, nil)
			statuses[i] = status
		}(index)
	}
	close(start)
	waitGroup.Wait()

	winners := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			winners++
		case http.StatusBadRequest:
		default:
			t.Fatalf("并发重置出现意外状态: %v", statuses)
		}
	}
	if winners != 1 {
		t.Fatalf("同一令牌并发重置只能有一个赢家: %v", statuses)
	}
	state := passwordLoadState(t, app, "resrace@example.com")
	if state.version != 1 {
		t.Fatalf("并发重置后 sessionVersion 应为 1: %d", state.version)
	}
	// 只有赢家的密码可以登录。
	loggedIn := false
	for _, password := range passwords {
		login := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
			map[string]string{"email": "resrace@example.com", "password": password}, nil)
		if login.status == http.StatusOK {
			loggedIn = true
		}
	}
	if !loggedIn {
		t.Fatalf("赢家的新密码必须可登录")
	}
}

func TestForgotPasswordReissueInvalidatesPreviousToken(t *testing.T) {
	app, capture := coreMailApp(t)
	coreRegister(t, app, "reissue@example.com", "secret123")

	first := coreRequestRecoveryToken(t, app, capture, "reissue@example.com")
	second := coreRequestRecoveryToken(t, app, capture, "reissue@example.com")
	if first == second {
		t.Fatalf("重新申请必须生成新令牌")
	}
	if count := coreAccountTokenCount(t, app, 1); count != 1 {
		t.Fatalf("同一账号每种用途至多一条令牌: %d", count)
	}

	stale := coreResetPassword(t, app, first, "brand-new-secret")
	if stale.status != http.StatusBadRequest || coreErrorCode(t, stale) != "INVALID_TOKEN" {
		t.Fatalf("旧链接必须失效: %d %v", stale.status, stale.body)
	}
	if response := coreResetPassword(t, app, second, "brand-new-secret"); response.status != http.StatusOK {
		t.Fatalf("最新令牌应当可用: %d %v", response.status, response.body)
	}
}

func TestAdminLoginFailsClosedWhenAuditCannotBeWritten(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "auditfail@example.com", "secret123")

	// 审计写入失败时管理员会话不得建立（管理员登录必须可审计）。
	if _, err := app.db.Exec(`DROP TABLE "AuditLog"`); err != nil {
		t.Fatalf("删除审计表失败: %v", err)
	}

	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "auditfail@example.com", "password": "secret123"}, nil)
	if response.status != http.StatusInternalServerError {
		t.Fatalf("审计失败时管理员登录应当 500: %d %v", response.status, response.body)
	}
	for _, cookie := range response.cookies {
		if cookie.Name == app.cfg.AuthCookieName && cookie.Value != "" && cookie.MaxAge > 0 {
			t.Fatalf("审计失败时不得下发会话 Cookie: %+v", cookie)
		}
	}
}
