package server

// 激活码管理接口的回归：生成 → 注册消费互通、状态筛选、撤销幂等/冲突、
// 过期处理、搜索与分页边界。生成与注册共用 auth_register.go 的
// normalizeActivationCode / hashActivationCode，因此这里用真实注册接口验证互通。

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var adminTestActivationCodePattern = regexp.MustCompile(`^[A-Z2-9]{4}(-[A-Z2-9]{4}){4}$`)

func adminTestGeneratedCodes(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	codes := adminTestListItems(t, body, "codes")
	if len(codes) == 0 {
		t.Fatalf("响应没有生成任何激活码: %v", body)
	}
	return codes
}

func TestAdminActivationCodesGenerateAndRegister(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	recorder := env.doJSON(t, http.MethodPost, "/api/admin/activation-codes",
		map[string]any{"count": 2, "expiresInDays": 7, "note": "给朋友"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("生成激活码应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	generated := adminTestGeneratedCodes(t, body)
	if len(generated) != 2 {
		t.Fatalf("应生成 2 个激活码，实际 %d", len(generated))
	}
	seen := make(map[string]struct{}, 2)
	for _, entry := range generated {
		code := adminTestString(t, entry, "code")
		if !adminTestActivationCodePattern.MatchString(code) {
			t.Fatalf("激活码格式应为 ABCD-EFGH-JKMN-PQRS-TVWX，实际 %q", code)
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("同一次生成不应出现重复激活码: %q", code)
		}
		seen[code] = struct{}{}
		masked := adminTestString(t, entry, "maskedCode")
		if masked != adminMaskActivationCode(normalizeActivationCode(code)) {
			t.Fatalf("maskedCode 与激活码不匹配: %q vs %q", masked, code)
		}
		if expires, ok := entry["expiresAt"].(string); !ok || expires == "" {
			t.Fatalf("expiresInDays=7 时 expiresAt 应为时间戳，实际 %v", entry["expiresAt"])
		}
	}

	// 库里只存 hash + 掩码：完整码只在生成响应里出现一次。
	for _, entry := range generated {
		code := adminTestString(t, entry, "code")
		normalized := normalizeActivationCode(code)
		var hash, masked string
		if err := env.app.db.QueryRow(`SELECT codeHash, maskedCode FROM "ActivationCode" WHERE codeHash = ?`,
			hashActivationCode(normalized)).Scan(&hash, &masked); err != nil {
			t.Fatalf("数据库里找不到激活码哈希（生成与消费规范不一致？）: %v", err)
		}
		if hash != hashActivationCode(normalized) || len(hash) != 64 {
			t.Fatalf("激活码必须以 sha256 十六进制存储，实际 %q", hash)
		}
		if !strings.Contains(masked, "****") || strings.Contains(masked, normalized) {
			t.Fatalf("库里的掩码不能包含完整激活码: %q", masked)
		}
	}

	create := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminActivationCodeCreate), auditActionAdminActivationCodeCreate)
	if !strings.Contains(create.Detail.String, "count=2") || !strings.Contains(create.Detail.String, "给朋友") {
		t.Fatalf("生成审计应记录数量与备注: %q", create.Detail.String)
	}

	// 列表：完整码不再出现，只有掩码；未使用 2 个。
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?status=unused", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("激活码列表应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	listBody := adminTestJSON(t, recorder)
	if total := adminTestNumber(t, listBody, "total"); total != 2 {
		t.Fatalf("unused 应为 2，实际 %d", total)
	}
	if page := adminTestNumber(t, listBody, "page"); page != 1 {
		t.Fatalf("page 应为 1，实际 %d", page)
	}
	if pages := adminTestNumber(t, listBody, "totalPages"); pages != 1 {
		t.Fatalf("totalPages 应为 1，实际 %d", pages)
	}
	rows := adminTestListItems(t, listBody, "codes")
	for _, row := range rows {
		if _, ok := row["code"]; ok {
			t.Fatalf("列表不能返回完整激活码: %v", row)
		}
		masked := adminTestString(t, row, "maskedCode")
		if !strings.Contains(masked, "****") {
			t.Fatalf("列表应显示掩码，实际 %q", masked)
		}
		creator, ok := row["createdBy"].(map[string]any)
		if !ok {
			t.Fatalf("列表缺少 createdBy: %v", row)
		}
		if email := adminTestString(t, creator, "email"); email != "root@example.com" {
			t.Fatalf("createdBy.email 应为管理员邮箱，实际 %q", email)
		}
		if row["usedBy"] != nil {
			t.Fatalf("未使用的激活码 usedBy 必须为 null: %v", row["usedBy"])
		}
	}

	// 用生成的码注册：与 auth_register.go 的消费逻辑必须互通（码生成方与消费方哈希一致）。
	firstCode := adminTestString(t, generated[0], "code")
	register := env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "friend@example.com", "password": "friend-pass", "activationCode": firstCode},
		adminTestAuth{})
	if register.Code != http.StatusOK {
		t.Fatalf("用管理端生成的激活码注册应成功，实际 %d: %s", register.Code, register.Body.String())
	}
	registerBody := adminTestJSON(t, register)
	registered, _ := registerBody["user"].(map[string]any)
	if role := adminTestString(t, registered, "role"); role != roleUser {
		t.Fatalf("激活码注册的账号角色应为 USER，实际 %q", role)
	}
	registeredID := adminTestNumber(t, registered, "id")

	// 同一码再次注册 → 无效。
	register = env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "friend2@example.com", "password": "friend-pass", "activationCode": firstCode},
		adminTestAuth{})
	if register.Code != http.StatusBadRequest || adminTestErrorCode(t, register) != "INVALID_ACTIVATION_CODE" {
		t.Fatalf("已使用的激活码应被拒绝（400 INVALID_ACTIVATION_CODE），实际 %d %s", register.Code, register.Body.String())
	}

	// 注册时写 ACTIVATION_CODE_USED：actor 是消费激活码的新账号（与原版一致）。
	used := adminTestMustOne(t, env.auditRowsFor(t, auditActionActivationCodeUsed), auditActionActivationCodeUsed)
	if used.ActorID != registeredID {
		t.Fatalf("ACTIVATION_CODE_USED 的 actor 应为新账号 %d，实际 %d", registeredID, used.ActorID)
	}
	if !strings.Contains(used.Detail.String, "codeId=") {
		t.Fatalf("ACTIVATION_CODE_USED 审计应记录 codeId: %q", used.Detail.String)
	}

	// 状态筛选：used 1 个、unused 1 个，usedBy 指向新账号。
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?status=used", nil, adminTestCookieAuth(adminCookie))
	listBody = adminTestJSON(t, recorder)
	if total := adminTestNumber(t, listBody, "total"); total != 1 {
		t.Fatalf("used 应为 1，实际 %d", total)
	}
	usedRows := adminTestListItems(t, listBody, "codes")
	usedBy, ok := usedRows[0]["usedBy"].(map[string]any)
	if !ok {
		t.Fatalf("已使用激活码应带 usedBy: %v", usedRows[0])
	}
	if email := adminTestString(t, usedBy, "email"); email != "friend@example.com" {
		t.Fatalf("usedBy.email 应为消费账号，实际 %q", email)
	}

	// 撤销已使用的激活码 → 409。
	usedID := adminTestString(t, usedRows[0], "id")
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/"+usedID,
		map[string]string{"operation": "revoke"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusConflict || adminTestErrorCode(t, recorder) != "ACTIVATION_CODE_USED" {
		t.Fatalf("撤销已使用激活码应为 409 ACTIVATION_CODE_USED，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 撤销未使用激活码 → changed=true；重复撤销 → changed=false（no-op），两者都写审计。
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?status=unused", nil, adminTestCookieAuth(adminCookie))
	listBody = adminTestJSON(t, recorder)
	unusedRows := adminTestListItems(t, listBody, "codes")
	if len(unusedRows) != 1 {
		t.Fatalf("此时应剩 1 个可用激活码，实际 %d", len(unusedRows))
	}
	unusedID := adminTestString(t, unusedRows[0], "id")
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/"+unusedID,
		map[string]string{"operation": "revoke"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK || !adminTestBool(t, adminTestJSON(t, recorder), "changed") {
		t.Fatalf("首次撤销应 200 changed=true，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/"+unusedID,
		map[string]string{"operation": "revoke"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK || adminTestBool(t, adminTestJSON(t, recorder), "changed") {
		t.Fatalf("重复撤销应 200 changed=false，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	revokeAudits := env.auditRowsFor(t, auditActionAdminActivationCodeRevoke)
	if len(revokeAudits) != 2 {
		t.Fatalf("两次撤销都应写审计，实际 %d", len(revokeAudits))
	}
	if !strings.Contains(revokeAudits[0].Detail.String, "changed=true") ||
		!strings.Contains(revokeAudits[1].Detail.String, "changed=false") {
		t.Fatalf("撤销审计应记录 changed：%q / %q", revokeAudits[0].Detail.String, revokeAudits[1].Detail.String)
	}

	// 撤销后的码不能用于注册。
	revokedCode := adminTestString(t, generated[1], "code")
	register = env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "friend3@example.com", "password": "friend-pass", "activationCode": revokedCode},
		adminTestAuth{})
	if register.Code != http.StatusBadRequest || adminTestErrorCode(t, register) != "INVALID_ACTIVATION_CODE" {
		t.Fatalf("已撤销激活码应被拒绝，实际 %d %s", register.Code, register.Body.String())
	}

	// 后续账号没有激活码 → ACTIVATION_CODE_REQUIRED（首账号免码的分支仍然存在）。
	register = env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "friend4@example.com", "password": "friend-pass"}, adminTestAuth{})
	if register.Code != http.StatusBadRequest || adminTestErrorCode(t, register) != "ACTIVATION_CODE_REQUIRED" {
		t.Fatalf("缺少激活码应为 400 ACTIVATION_CODE_REQUIRED，实际 %d %s", register.Code, register.Body.String())
	}
}

func TestAdminActivationCodesValidation(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	invalidCount := []map[string]any{
		{"count": 0},
		{"count": 51},
		{"count": 1.5},
		{"count": "3"},
		{},
	}
	for _, payload := range invalidCount {
		recorder := env.doJSON(t, http.MethodPost, "/api/admin/activation-codes", payload, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_COUNT" {
			t.Fatalf("%v 应为 400 INVALID_COUNT，实际 %d %s", payload, recorder.Code, recorder.Body.String())
		}
	}
	for _, payload := range []map[string]any{
		{"count": 1, "expiresInDays": -1},
		{"count": 1, "expiresInDays": 3651},
		{"count": 1, "expiresInDays": 1.5},
		{"count": 1, "expiresInDays": "7"},
	} {
		recorder := env.doJSON(t, http.MethodPost, "/api/admin/activation-codes", payload, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_EXPIRES" {
			t.Fatalf("%v 应为 400 INVALID_EXPIRES，实际 %d %s", payload, recorder.Code, recorder.Body.String())
		}
	}
	recorder := env.do(t, http.MethodPost, "/api/admin/activation-codes", strings.NewReader(`null`), adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_BODY" {
		t.Fatalf("null 请求体应为 400 INVALID_BODY，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 上限 50 与 0=永久都要真的生效。
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/activation-codes",
		map[string]any{"count": 50, "expiresInDays": 0}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("生成 50 个上限应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	codes := adminTestGeneratedCodes(t, adminTestJSON(t, recorder))
	if len(codes) != 50 {
		t.Fatalf("应生成 50 个激活码，实际 %d", len(codes))
	}
	for _, entry := range codes {
		if value, present := entry["expiresAt"]; !present || value != nil {
			t.Fatalf("expiresInDays=0 应为永久（expiresAt=null），实际 %v", value)
		}
	}

	// 备注超过 200 字会被截断（数据库里不存超长备注）。
	longNote := strings.Repeat("备", 260)
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/activation-codes",
		map[string]any{"count": 1, "note": longNote}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("长备注应被截断而不是报错，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var storedNote string
	if err := env.app.db.QueryRow(`SELECT note FROM "ActivationCode" WHERE note IS NOT NULL ORDER BY createdAt DESC, maskedCode DESC LIMIT 1`).
		Scan(&storedNote); err != nil {
		t.Fatalf("读取备注失败: %v", err)
	}
	if runes := []rune(storedNote); len(runes) != adminActivationMaxNoteLength {
		t.Fatalf("备注应截断到 %d 字，实际 %d", adminActivationMaxNoteLength, len(runes))
	}

	// 非法状态筛选。
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?status=bogus", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_STATUS" {
		t.Fatalf("非法 status 应为 400 INVALID_STATUS，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 撤销接口：非法 operation、未知 id。
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/whatever",
		map[string]string{"operation": "delete"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_OPERATION" {
		t.Fatalf("非法撤销 operation 应为 400 INVALID_OPERATION，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/does-not-exist",
		map[string]string{"operation": "revoke"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusNotFound || adminTestErrorCode(t, recorder) != "NOT_FOUND" {
		t.Fatalf("未知激活码应为 404 NOT_FOUND，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminActivationCodesExpirySearchAndPaging(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	past := coreTimestamp(time.Now().Add(-time.Hour))
	future := coreTimestamp(time.Now().Add(24 * time.Hour))
	expiredCode := env.addActivationCode(t, "AAAA-BBBB-CCCC-DDDD-EEEE", 1, coreNow(), &past, nil, nil)
	liveCode := env.addActivationCode(t, "FFFF-GGGG-HHHH-JJJJ-KKKK", 1, coreNow(), &future, nil, nil)
	env.addActivationCode(t, "MMMM-NNNN-PPPP-QQQQ-RRRR", 1, coreNow(), nil, nil, nil)

	// 过期码不能注册，未过期码可以。
	register := env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "expired@example.com", "password": "expired-pass", "activationCode": "AAAA-BBBB-CCCC-DDDD-EEEE"},
		adminTestAuth{})
	if register.Code != http.StatusBadRequest || adminTestErrorCode(t, register) != "INVALID_ACTIVATION_CODE" {
		t.Fatalf("过期激活码应被拒绝，实际 %d %s", register.Code, register.Body.String())
	}

	statusCount := func(t *testing.T, status string) (int64, []map[string]any) {
		t.Helper()
		recorder := env.do(t, http.MethodGet, "/api/admin/activation-codes?status="+status+"&limit=100", nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%s 应为 200，实际 %d: %s", status, recorder.Code, recorder.Body.String())
		}
		body := adminTestJSON(t, recorder)
		return adminTestNumber(t, body, "total"), adminTestListItems(t, body, "codes")
	}

	if total, _ := statusCount(t, "expired"); total != 1 {
		t.Fatalf("expired 应为 1，实际 %d", total)
	}
	unusedTotal, _ := statusCount(t, "unused")
	if unusedTotal != 2 {
		t.Fatalf("unused 应为 2（永久 + 未过期），实际 %d", unusedTotal)
	}

	// 未过期的激活码仍可注册（过期判断不能误伤有效码）。
	register = env.doJSON(t, http.MethodPost, "/api/auth/register",
		map[string]string{"email": "live@example.com", "password": "live-pass", "activationCode": "FFFF-GGGG-HHHH-JJJJ-KKKK"},
		adminTestAuth{})
	if register.Code != http.StatusOK {
		t.Fatalf("未过期激活码应能注册，实际 %d: %s", register.Code, register.Body.String())
	}
	if liveCode == "" {
		t.Fatalf("liveCode 不应为空")
	}

	// 搜索：掩码片段与备注都必须能命中。
	recorder := env.do(t, http.MethodGet, "/api/admin/activation-codes?q=MMMM", nil, adminTestCookieAuth(adminCookie))
	body := adminTestJSON(t, recorder)
	if total := adminTestNumber(t, body, "total"); total != 1 {
		t.Fatalf("掩码搜索应命中 1 个，实际 %d", total)
	}
	if _, err := env.app.db.Exec(`UPDATE "ActivationCode" SET note = 'beijing-invite' WHERE id = ?`, expiredCode); err != nil {
		t.Fatalf("写入备注失败: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?q=beijing", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if total := adminTestNumber(t, body, "total"); total != 1 {
		t.Fatalf("备注搜索应命中 1 个，实际 %d", total)
	}

	// 同一秒创建的多行：分页互不重叠、不丢失。
	sameCreatedAt := coreTimestamp(time.Now())
	for index := 0; index < 3; index++ {
		env.addActivationCode(t, fmt.Sprintf("ZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZ%d", index), 1, sameCreatedAt, nil, nil, nil)
	}
	collect := func(t *testing.T, page int) []string {
		t.Helper()
		recorder := env.do(t, http.MethodGet,
			fmt.Sprintf("/api/admin/activation-codes?page=%d&limit=2", page), nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("分页请求应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
		rows := adminTestListItems(t, adminTestJSON(t, recorder), "codes")
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, adminTestString(t, row, "id"))
		}
		return ids
	}
	pageOne := collect(t, 1)
	pageTwo := collect(t, 2)
	pageThree := collect(t, 3)
	if len(pageOne) != 2 || len(pageTwo) != 2 {
		t.Fatalf("前两页应各 2 行，实际 %d / %d", len(pageOne), len(pageTwo))
	}
	seen := make(map[string]struct{}, 6)
	for _, id := range append(append(pageOne, pageTwo...), pageThree...) {
		if _, dup := seen[id]; dup {
			t.Fatalf("分页出现重复行: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 6 {
		t.Fatalf("同秒创建时三页应覆盖全部 6 条，实际 %d", len(seen))
	}

	// 页码越界夹到最后一页。
	recorder = env.do(t, http.MethodGet, "/api/admin/activation-codes?page=99&limit=100", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if page := adminTestNumber(t, body, "page"); page != 1 {
		t.Fatalf("page=99 应夹到 1，实际 %d", page)
	}
}
