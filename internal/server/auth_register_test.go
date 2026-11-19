package server

import (
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---- helpers for registration / account tests ----

// coreInsertUser 直接写入一个账号（绕过注册接口），用于构造第二、第三个账号。
func coreInsertUser(t *testing.T, app *App, email, password, role string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		t.Fatalf("生成密码哈希失败: %v", err)
	}
	result, err := app.db.Exec(
		`INSERT INTO "User" (email, username, passwordHash, role, disabledAt, sessionVersion, mustChangePassword, quotaBytes, createdAt)
		 VALUES (?, ?, ?, ?, NULL, 0, 0, NULL, ?)`,
		email, newInternalUsername(), string(hash), role, coreNow())
	if err != nil {
		t.Fatalf("插入账号 %s 失败: %v", email, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("读取账号 ID 失败: %v", err)
	}
	return id
}

// coreInsertActivationCode 直接写入激活码（管理端生成接口由另一组 worker 实现）。
func coreInsertActivationCode(t *testing.T, app *App, createdBy int64, code string, expiresAt, revokedAt *string) string {
	t.Helper()
	id := "code_" + hashActivationCode(normalizeActivationCode(code))[:12]
	var expires, revoked sql.NullString
	if expiresAt != nil {
		expires = sql.NullString{String: *expiresAt, Valid: true}
	}
	if revokedAt != nil {
		revoked = sql.NullString{String: *revokedAt, Valid: true}
	}
	if _, err := app.db.Exec(
		`INSERT INTO "ActivationCode" (id, codeHash, maskedCode, note, createdById, usedById, createdAt, expiresAt, usedAt, revokedAt)
		 VALUES (?, ?, ?, NULL, ?, NULL, ?, ?, NULL, ?)`,
		id, hashActivationCode(normalizeActivationCode(code)), "ABCD-****", createdBy, coreNow(), expires, revoked); err != nil {
		t.Fatalf("插入激活码失败: %v", err)
	}
	return id
}

func coreRegisterWithCode(t *testing.T, app *App, email, password, code string) *coreResponse {
	t.Helper()
	payload := map[string]any{"email": email, "password": password}
	if code != "" {
		payload["activationCode"] = code
	}
	return coreDo(t, app.Handler(), http.MethodPost, "/api/auth/register", payload, nil)
}

func coreActivationCodeState(t *testing.T, app *App, id string) (usedBy sql.NullInt64, usedAt sql.NullString) {
	t.Helper()
	if err := app.db.QueryRow(
		`SELECT usedById, usedAt FROM "ActivationCode" WHERE id = ?`, id).Scan(&usedBy, &usedAt); err != nil {
		t.Fatalf("读取激活码状态失败: %v", err)
	}
	return usedBy, usedAt
}

func coreAuditRows(t *testing.T, app *App, action string) []map[string]any {
	t.Helper()
	rows, err := app.db.Query(
		`SELECT actorId, action, targetUserId, detail, createdAt FROM "AuditLog" WHERE action = ? ORDER BY id`, action)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	defer rows.Close()

	var logs []map[string]any
	for rows.Next() {
		var (
			actorID  int64
			act      string
			targetID sql.NullInt64
			detail   sql.NullString
			created  string
		)
		if err := rows.Scan(&actorID, &act, &targetID, &detail, &created); err != nil {
			t.Fatalf("读取审计行失败: %v", err)
		}
		logs = append(logs, map[string]any{
			"actorId":      actorID,
			"action":       act,
			"targetUserId": targetID,
			"detail":       detail,
			"createdAt":    created,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计失败: %v", err)
	}
	return logs
}

func TestRegistrationFirstUserBecomesAdminWithoutActivationCode(t *testing.T) {
	app := coreTestApp(t, Config{})
	response, cookie := coreRegister(t, app, "first-admin@example.com", "secret123")
	user, _ := response.body["user"].(map[string]any)
	if user["role"] != roleAdmin {
		t.Fatalf("首个账号必须是管理员: %v", user)
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("首个账号注册后应直接登录")
	}
	if logs := coreAuditRows(t, app, auditActionActivationCodeUsed); len(logs) != 0 {
		t.Fatalf("首个账号免码注册不应写激活码审计: %v", logs)
	}
}

func TestRegistrationRequiresActivationCodeAfterFirstUser(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "admin@example.com", "secret123")

	response := coreRegisterWithCode(t, app, "second@example.com", "secret123", "")
	if response.status != http.StatusBadRequest || coreErrorCode(t, response) != "ACTIVATION_CODE_REQUIRED" {
		t.Fatalf("后续注册必须要求激活码: %d %v", response.status, response.body)
	}
	if total, _ := coreUserCount(t, app); total != 1 {
		t.Fatalf("缺少激活码时不得创建账号: %d", total)
	}

	// 空白/只有分隔符的激活码同样视为未提供。
	blank := coreRegisterWithCode(t, app, "second@example.com", "secret123", " - - ")
	if blank.status != http.StatusBadRequest || coreErrorCode(t, blank) != "ACTIVATION_CODE_REQUIRED" {
		t.Fatalf("空白激活码应按未提供处理: %d %v", blank.status, blank.body)
	}
}

func TestRegistrationConsumesActivationCodeAndWritesAudit(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, adminCookie := coreRegister(t, app, "admin@example.com", "secret123")
	var adminID int64
	if err := app.db.QueryRow(`SELECT id FROM "User" WHERE email = ?`, "admin@example.com").Scan(&adminID); err != nil {
		t.Fatalf("读取管理员 ID 失败: %v", err)
	}
	code := "ABCD-EFGH-JKMN-PQRS-TVWX"
	codeID := coreInsertActivationCode(t, app, adminID, code, nil, nil)

	// 注册输入大小写/分隔符不敏感。
	response := coreRegisterWithCode(t, app, "member@example.com", "secret123", "abcd efgh jkmn pqrs tvwx")
	if response.status != http.StatusOK {
		t.Fatalf("有效激活码应当注册成功: %d %v", response.status, response.body)
	}
	user, _ := response.body["user"].(map[string]any)
	if user["role"] != roleUser {
		t.Fatalf("激活码注册的账号应为普通用户: %v", user)
	}
	memberID := int64(user["id"].(float64))

	usedBy, usedAt := coreActivationCodeState(t, app, codeID)
	if !usedAt.Valid || !usedBy.Valid || usedBy.Int64 != memberID {
		t.Fatalf("激活码必须记录使用时间与使用者: usedBy=%v usedAt=%v", usedBy, usedAt)
	}

	logs := coreAuditRows(t, app, auditActionActivationCodeUsed)
	if len(logs) != 1 {
		t.Fatalf("应当写入一条 ACTIVATION_CODE_USED 审计: %v", logs)
	}
	if logs[0]["actorId"] != memberID {
		t.Fatalf("审计 actor 必须是新注册的用户: %v", logs[0])
	}
	detail, _ := logs[0]["detail"].(sql.NullString)
	if !detail.Valid || !strings.Contains(detail.String, "codeId="+codeID) {
		t.Fatalf("审计 detail 应包含 codeId: %v", logs[0])
	}

	// 复用同一个激活码必须失败，且不创建第三个账号。
	reuse := coreRegisterWithCode(t, app, "reuse@example.com", "secret123", code)
	if reuse.status != http.StatusBadRequest || coreErrorCode(t, reuse) != "INVALID_ACTIVATION_CODE" {
		t.Fatalf("复用激活码必须被拒绝: %d %v", reuse.status, reuse.body)
	}
	if total, _ := coreUserCount(t, app); total != 2 {
		t.Fatalf("复用激活码不得创建账号: %d", total)
	}

	// 管理员仍然可用：注册流程不影响已有会话。
	if me := coreDo(t, app.Handler(), http.MethodGet, "/api/auth/me", nil, coreCookieHeader(adminCookie)); me.status != http.StatusOK {
		t.Fatalf("注册后管理员会话应继续有效: %d %v", me.status, me.body)
	}
}

func TestRegistrationRejectsUnknownRevokedAndExpiredCodes(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "admin@example.com", "secret123")
	var adminID int64
	if err := app.db.QueryRow(`SELECT id FROM "User" WHERE email = ?`, "admin@example.com").Scan(&adminID); err != nil {
		t.Fatalf("读取管理员 ID 失败: %v", err)
	}

	past := coreTimestamp(time.Now().Add(-time.Hour))
	revokedAt := coreNow()
	revokedID := coreInsertActivationCode(t, app, adminID, "REVOKED-CODE-ABCD-EFGH", nil, &revokedAt)
	expiredID := coreInsertActivationCode(t, app, adminID, "EXPIRED-CODE-ABCD-EFGH", &past, nil)

	cases := map[string]string{
		"unknown": "UNKNOWN-CODE-ABCD-EFGH",
		"revoked": "REVOKED-CODE-ABCD-EFGH",
		"expired": "EXPIRED-CODE-ABCD-EFGH",
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			response := coreRegisterWithCode(t, app, name+"@example.com", "secret123", code)
			if response.status != http.StatusBadRequest || coreErrorCode(t, response) != "INVALID_ACTIVATION_CODE" {
				t.Fatalf("%s 激活码必须被拒绝: %d %v", name, response.status, response.body)
			}
		})
	}

	if total, _ := coreUserCount(t, app); total != 1 {
		t.Fatalf("无效激活码不得创建账号: %d", total)
	}
	for _, id := range []string{revokedID, expiredID} {
		usedBy, usedAt := coreActivationCodeState(t, app, id)
		if usedAt.Valid || usedBy.Valid {
			t.Fatalf("被拒绝的激活码不得被消费: id=%s usedBy=%v usedAt=%v", id, usedBy, usedAt)
		}
	}
}

func TestRegistrationEmailConflictRollsBackActivationCode(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "admin@example.com", "secret123")
	var adminID int64
	if err := app.db.QueryRow(`SELECT id FROM "User" WHERE email = ?`, "admin@example.com").Scan(&adminID); err != nil {
		t.Fatalf("读取管理员 ID 失败: %v", err)
	}
	codeID := coreInsertActivationCode(t, app, adminID, "CONFLICT-CODE-ABCD-EFGH", nil, nil)

	response := coreRegisterWithCode(t, app, "ADMIN@example.com", "secret123", "CONFLICT-CODE-ABCD-EFGH")
	if response.status != http.StatusConflict || coreErrorCode(t, response) != "EMAIL_EXISTS" {
		t.Fatalf("已注册邮箱应返回 409 EMAIL_EXISTS: %d %v", response.status, response.body)
	}

	usedBy, usedAt := coreActivationCodeState(t, app, codeID)
	if usedAt.Valid || usedBy.Valid {
		t.Fatalf("邮箱冲突导致回滚时激活码不得被消费: usedBy=%v usedAt=%v", usedBy, usedAt)
	}
	if logs := coreAuditRows(t, app, auditActionActivationCodeUsed); len(logs) != 0 {
		t.Fatalf("回滚的注册不得写审计: %v", logs)
	}
}

func TestRegistrationConcurrentSameCodeHasSingleWinner(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "admin@example.com", "secret123")
	var adminID int64
	if err := app.db.QueryRow(`SELECT id FROM "User" WHERE email = ?`, "admin@example.com").Scan(&adminID); err != nil {
		t.Fatalf("读取管理员 ID 失败: %v", err)
	}
	codeID := coreInsertActivationCode(t, app, adminID, "CONCURRENT-CODE-ABCD-EFGH-2345", nil, nil)

	const attempts = 2
	emails := []string{"race-a@example.com", "race-b@example.com"}
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		statuses  = make([]int, attempts)
		bodies    = make([]map[string]any, attempts)
		start     = make(chan struct{})
	)
	for i := 0; i < attempts; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			status, parsed := testPasswordDo(app.Handler(), "/api/auth/register",
				`{"email":"`+emails[index]+`","password":"secret123","activationCode":"CONCURRENT-CODE-ABCD-EFGH-2345"}`, nil)
			mu.Lock()
			statuses[index] = status
			bodies[index] = parsed
			mu.Unlock()
		}(i)
	}
	close(start)
	waitGroup.Wait()

	successes := 0
	for index, status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest, http.StatusConflict:
		default:
			t.Fatalf("并发注册出现意外状态 %d: %v", status, bodies[index])
		}
	}
	if successes != 1 {
		t.Fatalf("同一激活码并发注册只能有一个赢家: %v %v", statuses, bodies)
	}
	if total, _ := coreUserCount(t, app); total != 2 {
		t.Fatalf("并发注册只应新增一个账号: %d", total)
	}
	usedBy, usedAt := coreActivationCodeState(t, app, codeID)
	if !usedAt.Valid || !usedBy.Valid {
		t.Fatalf("赢家必须消费激活码: usedBy=%v usedAt=%v", usedBy, usedAt)
	}
	if logs := coreAuditRows(t, app, auditActionActivationCodeUsed); len(logs) != 1 {
		t.Fatalf("并发注册只应写一条激活码审计: %v", logs)
	}
}

func TestAdminLoginWritesAudit(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "admin@example.com", "secret123")

	if logs := coreAuditRows(t, app, auditActionAdminLogin); len(logs) != 0 {
		t.Fatalf("注册不会自动写 ADMIN_LOGIN 审计: %v", logs)
	}

	login := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "admin@example.com", "password": "secret123"}, nil)
	if login.status != http.StatusOK {
		t.Fatalf("管理员登录失败: %d %v", login.status, login.body)
	}
	if logs := coreAuditRows(t, app, auditActionAdminLogin); len(logs) != 1 {
		t.Fatalf("每次管理员登录都应写审计: %v", logs)
	}

	// 普通用户登录不写 ADMIN_LOGIN。
	coreInsertUser(t, app, "plain@example.com", "secret123", roleUser)
	plain := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "plain@example.com", "password": "secret123"}, nil)
	if plain.status != http.StatusOK {
		t.Fatalf("普通用户登录失败: %d %v", plain.status, plain.body)
	}
	if logs := coreAuditRows(t, app, auditActionAdminLogin); len(logs) != 1 {
		t.Fatalf("普通用户登录不应写 ADMIN_LOGIN: %v", logs)
	}
}
