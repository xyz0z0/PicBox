package server

// 账号管理接口的 HTTP 级回归：列表分页/搜索、建号一次性临时密码、六个 PATCH operation
// （禁用/启用、重置密码、解除导出限制、改配额、改邮箱）、审计写入与失败回滚。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func adminTestFindUser(t *testing.T, users []map[string]any, id int64) map[string]any {
	t.Helper()
	for _, item := range users {
		if adminTestNumber(t, item, "id") == id {
			return item
		}
	}
	t.Fatalf("用户列表中找不到 id=%d: %v", id, users)
	return nil
}

func TestAdminUsersListPaginationAndUsage(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	firstID, _ := env.addUser(t, "alice@example.com")
	secondID, _ := env.addUser(t, "bob@example.com")
	env.addAPIKey(t, firstID)

	trashedAt := imagesNow()
	env.addImage(t, firstID, "kept.png", adminTestImageOptions{Size: 2048})
	env.addImage(t, firstID, "trashed.png", adminTestImageOptions{Size: 1024, DeletedAt: &trashedAt})
	env.addImage(t, secondID, "other.png", adminTestImageOptions{Size: 512})
	// 管理员下架的图片同样计入用量：物理删除前仍占磁盘。
	env.addImage(t, firstID, "banned.png", adminTestImageOptions{Size: 4096, DeletedAt: &trashedAt, DeletedByAdmin: 1})

	recorder := env.do(t, http.MethodGet, "/api/admin/users", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("用户列表应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	if total := adminTestNumber(t, body, "total"); total != 3 {
		t.Fatalf("total 应为 3，实际 %d", total)
	}
	if page := adminTestNumber(t, body, "page"); page != 1 {
		t.Fatalf("page 应为 1，实际 %d", page)
	}
	if size := adminTestNumber(t, body, "pageSize"); size != 20 {
		t.Fatalf("pageSize 应为默认 20，实际 %d", size)
	}
	if pages := adminTestNumber(t, body, "totalPages"); pages != 1 {
		t.Fatalf("totalPages 应为 1，实际 %d", pages)
	}
	if _, ok := body["defaultQuotaBytes"]; !ok {
		t.Fatalf("响应缺少 defaultQuotaBytes: %v", body)
	}
	if _, ok := body["quotaBytes"]; !ok {
		t.Fatalf("响应缺少 quotaBytes: %v", body)
	}

	users := adminTestListItems(t, body, "users")
	if len(users) != 3 {
		t.Fatalf("users 长度应为 3，实际 %d", len(users))
	}
	// createdAt 相同（同一秒插入），按 id 降序兜底：最后创建的账号必须排最前。
	if adminTestNumber(t, users[0], "id") != secondID {
		t.Fatalf("列表应按 createdAt desc, id desc 排序，实际首行 id=%v", users[0]["id"])
	}

	first := adminTestFindUser(t, users, firstID)
	if used := adminTestNumber(t, first, "storageUsedBytes"); used != 2048+1024+4096 {
		t.Fatalf("storageUsedBytes 必须包含回收站与管理员下架，期望 7168，实际 %d", used)
	}
	counts, ok := first["_count"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 _count: %v", first)
	}
	if images := adminTestNumber(t, counts, "images"); images != 3 {
		t.Fatalf("_count.images 应为 3，实际 %d", images)
	}
	if keys := adminTestNumber(t, counts, "apiKeys"); keys != 1 {
		t.Fatalf("_count.apiKeys 应为 1，实际 %d", keys)
	}
	if override, present := first["quotaOverrideBytes"]; !present || override != nil {
		t.Fatalf("未设置覆盖时 quotaOverrideBytes 应为 null，实际 %v", override)
	}

	// 搜索：只匹配 email/username。
	recorder = env.do(t, http.MethodGet, "/api/admin/users?q=alice", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if total := adminTestNumber(t, body, "total"); total != 1 {
		t.Fatalf("q=alice 应只命中 1 个账号，实际 %d", total)
	}
	if len(adminTestListItems(t, body, "users")) != 1 {
		t.Fatalf("q=alice 应返回 1 行")
	}

	// 搜索特殊字符必须按字面量处理（LIKE 转义），不允许当作通配符匹配全部。
	recorder = env.do(t, http.MethodGet, "/api/admin/users?q=%25", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if total := adminTestNumber(t, body, "total"); total != 0 {
		t.Fatalf("q=%% 应命中 0 个账号，实际 %d", total)
	}

	// 页码夹到有效页。
	recorder = env.do(t, http.MethodGet, "/api/admin/users?page=99&limit=2", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if page := adminTestNumber(t, body, "page"); page != 2 {
		t.Fatalf("page=99 应夹到最后一页 2，实际 %d", page)
	}
	if pages := adminTestNumber(t, body, "totalPages"); pages != 2 {
		t.Fatalf("limit=2 时 totalPages 应为 2，实际 %d", pages)
	}
	if rows := adminTestListItems(t, body, "users"); len(rows) != 1 {
		t.Fatalf("最后一页应只剩 1 行，实际 %d", len(rows))
	}

	// limit 上下夹紧。
	recorder = env.do(t, http.MethodGet, "/api/admin/users?limit=0", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if size := adminTestNumber(t, body, "pageSize"); size != 1 {
		t.Fatalf("limit=0 应夹到 1，实际 %d", size)
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/users?limit=500", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if size := adminTestNumber(t, body, "pageSize"); size != 100 {
		t.Fatalf("limit=500 应夹到 100，实际 %d", size)
	}
}

func TestAdminUsersCreateTemporaryPassword(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	recorder := env.doJSON(t, http.MethodPost, "/api/admin/users",
		map[string]string{"email": "New.User@Example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("建号应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("建号响应必须 no-store，实际 %q", cache)
	}
	body := adminTestJSON(t, recorder)
	temporary := adminTestString(t, body, "temporaryPassword")
	if len(temporary) != adminTemporaryPasswordLength {
		t.Fatalf("临时密码长度应为 %d，实际 %d", adminTemporaryPasswordLength, len(temporary))
	}
	if strings.ContainsAny(temporary, "0O1lI") {
		t.Fatalf("临时密码不应包含易混字符: %q", temporary)
	}
	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 user: %v", body)
	}
	if !adminTestBool(t, user, "mustChangePassword") {
		t.Fatalf("管理员建号必须 mustChangePassword=true: %v", user)
	}
	if role := adminTestString(t, user, "role"); role != roleUser {
		t.Fatalf("管理员建号角色必须固定 USER，实际 %q", role)
	}
	if email := adminTestString(t, user, "email"); email != "new.user@example.com" {
		t.Fatalf("邮箱应被规范化，实际 %q", email)
	}
	username := adminTestString(t, user, "username")
	if !strings.HasPrefix(username, "user_") {
		t.Fatalf("内部 username 应为 user_<uuid> 格式，实际 %q", username)
	}
	userID := adminTestNumber(t, user, "id")

	// 一次性临时密码必须能登录，并且 /api/auth/me 报告必须改密。
	login := env.login(t, "new.user@example.com", temporary)
	if login.Code != http.StatusOK {
		t.Fatalf("临时密码登录失败: %d %s", login.Code, login.Body.String())
	}
	loginBody := adminTestJSON(t, login)
	loginUser, _ := loginBody["user"].(map[string]any)
	if !adminTestBool(t, loginUser, "mustChangePassword") {
		t.Fatalf("首次登录应要求改密: %v", loginUser)
	}

	created := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminUserCreate), auditActionAdminUserCreate)
	if !created.TargetUserID.Valid || created.TargetUserID.Int64 != userID {
		t.Fatalf("审计目标应为新建账号 %d，实际 %v", userID, created.TargetUserID)
	}
	if !strings.Contains(created.Detail.String, "must_change_password=true") {
		t.Fatalf("审计 detail 应记录 must_change_password: %q", created.Detail.String)
	}

	// 重复邮箱 → 409，且不新增账号、不新增审计。
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/users",
		map[string]string{"email": "new.user@example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("重复邮箱应为 409，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if code := adminTestErrorCode(t, recorder); code != "EMAIL_EXISTS" {
		t.Fatalf("重复邮箱错误码应为 EMAIL_EXISTS，实际 %q", code)
	}
	var userCount int64
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "User"`).Scan(&userCount); err != nil {
		t.Fatalf("读取账号数失败: %v", err)
	}
	if userCount != 2 {
		t.Fatalf("重复邮箱不应创建账号，当前账号数 %d", userCount)
	}

	// 非法邮箱与非法请求体。
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/users",
		map[string]string{"email": "not-an-email"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_EMAIL" {
		t.Fatalf("非法邮箱应为 400 INVALID_EMAIL，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.do(t, http.MethodPost, "/api/admin/users", strings.NewReader(`[]`), adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_BODY" {
		t.Fatalf("数组请求体应为 400 INVALID_BODY，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminUserDisableEnableAndSelfLock(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	adminID := int64(1)
	userID, userCookie := env.addUser(t, "plain@example.com")
	userKey := env.addAPIKey(t, userID)

	recorder := env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "disable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("禁用应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	user, _ := body["user"].(map[string]any)
	if disabled, ok := user["disabledAt"].(string); !ok || disabled == "" {
		t.Fatalf("禁用后 disabledAt 应为时间戳，实际 %v", user["disabledAt"])
	}

	// 有效 JWT 与 API Key 都必须立即不可用。
	if recorder := env.do(t, http.MethodGet, "/api/auth/me", nil, adminTestCookieAuth(userCookie)); recorder.Code != http.StatusForbidden {
		t.Fatalf("禁用账号的旧 JWT 应被拒绝（403），实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.do(t, http.MethodGet, "/api/images/usage", nil, adminTestAuth{bearer: userKey}); recorder.Code != http.StatusForbidden {
		t.Fatalf("禁用账号的 API Key 应被拒绝（403），实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.login(t, "plain@example.com", adminTestUserPassword); recorder.Code != http.StatusForbidden {
		t.Fatalf("禁用账号登录应为 403，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "enable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("启用应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	// sessionVersion 未变，原 Cookie 恢复可用。
	if recorder := env.do(t, http.MethodGet, "/api/auth/me", nil, adminTestCookieAuth(userCookie)); recorder.Code != http.StatusOK {
		t.Fatalf("启用后原 Cookie 应恢复可用，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 不允许通过禁用自己锁死唯一管理员入口。
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", adminID),
		map[string]string{"operation": "disable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusForbidden || adminTestErrorCode(t, recorder) != "FORBIDDEN" {
		t.Fatalf("禁用自己应为 403 FORBIDDEN，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	if disabled := env.userDisabledAt(t, adminID); disabled.Valid {
		t.Fatalf("禁用自己被拒绝后管理员必须仍然可用，实际 disabledAt=%v", disabled)
	}

	// 不存在的账号 → 404。
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/users/999999",
		map[string]string{"operation": "disable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusNotFound || adminTestErrorCode(t, recorder) != "NOT_FOUND" {
		t.Fatalf("不存在账号应为 404 NOT_FOUND，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 非法 operation 与非数字 id。
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "explode"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_OPERATION" {
		t.Fatalf("非法 operation 应为 400 INVALID_OPERATION，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/users/not-a-number",
		map[string]string{"operation": "disable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_USER_ID" {
		t.Fatalf("非法 id 应为 400 INVALID_USER_ID，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 审计：1 条禁用 + 1 条启用；被拒绝的"禁用自己"不写审计。
	disables := env.auditRowsFor(t, auditActionAdminUserDisable)
	if len(disables) != 1 {
		t.Fatalf("应只有 1 条禁用审计，实际 %d", len(disables))
	}
	if !disables[0].TargetUserID.Valid || disables[0].TargetUserID.Int64 != userID {
		t.Fatalf("禁用审计目标应为 %d，实际 %v", userID, disables[0].TargetUserID)
	}
	if enables := env.auditRowsFor(t, auditActionAdminUserEnable); len(enables) != 1 {
		t.Fatalf("应只有 1 条启用审计，实际 %d", len(enables))
	}
}

func TestAdminUserResetPasswordRevokesSessionsAndKeys(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, userCookie := env.addUser(t, "plain@example.com")
	userKey := env.addAPIKey(t, userID)

	// 非法密码不写库、不写审计。
	before := len(env.auditRows(t))
	recorder := env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "reset_password", "password": "123"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_PASSWORD" {
		t.Fatalf("过短密码应为 400 INVALID_PASSWORD，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	if after := len(env.auditRows(t)); after != before {
		t.Fatalf("被拒绝的重置密码不应写审计（%d → %d）", before, after)
	}

	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "reset_password", "password": "brand-new-pass"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("重置密码应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	if revoked := adminTestNumber(t, body, "revokedApiKeys"); revoked != 1 {
		t.Fatalf("应撤销 1 个 API Key，实际 %d", revoked)
	}
	user, _ := body["user"].(map[string]any)
	if version := adminTestNumber(t, user, "sessionVersion"); version != 1 {
		t.Fatalf("sessionVersion 应递增到 1，实际 %d", version)
	}

	if recorder := env.do(t, http.MethodGet, "/api/auth/me", nil, adminTestCookieAuth(userCookie)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("旧 JWT 应立即失效（401），实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.do(t, http.MethodGet, "/api/images/usage", nil, adminTestAuth{bearer: userKey}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("旧 API Key 应被撤销（401），实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.login(t, "plain@example.com", "brand-new-pass"); recorder.Code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	row := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminUserPasswordReset), auditActionAdminUserPasswordReset)
	if !strings.Contains(row.Detail.String, "session_version=1") || !strings.Contains(row.Detail.String, "api_keys_revoked=1") {
		t.Fatalf("审计 detail 应记录 session_version 与 api_keys_revoked: %q", row.Detail.String)
	}
}

func TestAdminUserQuotaVariants(t *testing.T) {
	env := adminTestNewApp(t, Config{UserQuotaBytes: 10 << 20})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, _ := env.addUser(t, "plain@example.com")
	trashedAt := imagesNow()
	env.addImage(t, userID, "a.png", adminTestImageOptions{Size: 2 << 20})
	env.addImage(t, userID, "b.png", adminTestImageOptions{Size: 1 << 20, DeletedAt: &trashedAt})

	path := fmt.Sprintf("/api/admin/users/%d", userID)
	call := func(t *testing.T, payload any) (*httptest.ResponseRecorder, map[string]any, map[string]any) {
		t.Helper()
		recorder := env.doJSON(t, http.MethodPatch, path, payload, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("配额请求应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
		body := adminTestJSON(t, recorder)
		quota, ok := body["quota"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 quota: %v", body)
		}
		return recorder, body, quota
	}

	// null → 继承全局；当前本来就是继承，因此 changed=false，但仍然写审计。
	_, _, quota := call(t, map[string]any{"operation": "set_quota", "quotaBytes": nil})
	if adminTestBool(t, quota, "changed") {
		t.Fatalf("继承全局的 no-op 必须 changed=false: %v", quota)
	}
	if effective := adminTestNumber(t, quota, "effectiveBytes"); effective != 10<<20 {
		t.Fatalf("继承时 effective 应为全局默认 10MiB，实际 %d", effective)
	}
	if override, present := quota["overrideBytes"]; !present || override != nil {
		t.Fatalf("继承时 overrideBytes 应为 null，实际 %v", override)
	}
	if used := adminTestNumber(t, quota, "usedBytes"); used != 3<<20 {
		t.Fatalf("用量必须包含回收站图片，期望 3MiB，实际 %d", used)
	}

	// 0 → 不限。
	_, _, quota = call(t, map[string]any{"operation": "set_quota", "quotaBytes": 0})
	if !adminTestBool(t, quota, "changed") {
		t.Fatalf("从继承改为 0 应为 changed=true: %v", quota)
	}
	if effective := adminTestNumber(t, quota, "effectiveBytes"); effective != 0 {
		t.Fatalf("0 表示不限，effective 应为 0，实际 %d", effective)
	}
	if exceeded := adminTestBool(t, quota, "exceeded"); exceeded {
		t.Fatalf("不限配额不应标记 exceeded: %v", quota)
	}

	// 正整数低于 1MiB → 抬到 1MiB，并因为当前用量更高而提示 exceeded。
	_, _, quota = call(t, map[string]any{"operation": "set_quota", "quotaBytes": 1024})
	if effective := adminTestNumber(t, quota, "effectiveBytes"); effective != adminMinUserQuotaBytes {
		t.Fatalf("低于 1MiB 的配额应抬到 1MiB，实际 %d", effective)
	}
	if override := adminTestNumber(t, quota, "overrideBytes"); override != adminMinUserQuotaBytes {
		t.Fatalf("overrideBytes 应记录抬高后的值，实际 %d", override)
	}
	if !adminTestBool(t, quota, "exceeded") {
		t.Fatalf("配额低于当前用量应标记 exceeded: %v", quota)
	}

	// 相同的覆盖值 → changed=false。
	_, _, quota = call(t, map[string]any{"operation": "set_quota", "quotaBytes": adminMinUserQuotaBytes})
	if adminTestBool(t, quota, "changed") {
		t.Fatalf("重复设置同一配额必须 changed=false: %v", quota)
	}

	// 4MiB 足够覆盖 3MiB 用量。
	_, _, quota = call(t, map[string]any{"operation": "set_quota", "quotaBytes": 4 << 20})
	if effective := adminTestNumber(t, quota, "effectiveBytes"); effective != 4<<20 {
		t.Fatalf("effective 应为 4MiB，实际 %d", effective)
	}
	if exceeded := adminTestBool(t, quota, "exceeded"); exceeded {
		t.Fatalf("用量低于配额不应 exceeded: %v", quota)
	}

	// 列表里必须出现同一份生效值与覆盖值。
	recorder := env.do(t, http.MethodGet, "/api/admin/users?q=plain", nil, adminTestCookieAuth(adminCookie))
	body := adminTestJSON(t, recorder)
	users := adminTestListItems(t, body, "users")
	if len(users) != 1 {
		t.Fatalf("应命中 1 个账号，实际 %d", len(users))
	}
	if value := adminTestNumber(t, users[0], "quotaBytes"); value != 4<<20 {
		t.Fatalf("列表 quotaBytes 应为生效值 4MiB，实际 %d", value)
	}
	if value := adminTestNumber(t, users[0], "quotaOverrideBytes"); value != 4<<20 {
		t.Fatalf("列表 quotaOverrideBytes 应为 4MiB，实际 %d", value)
	}

	// 非法值：字符串、小数、负数、缺字段、超范围浮点都要 400 且不改库。
	var beforeOverride interface{}
	if err := env.app.db.QueryRow(`SELECT quotaBytes FROM "User" WHERE id = ?`, userID).Scan(&beforeOverride); err != nil {
		t.Fatalf("读取配额失败: %v", err)
	}
	invalidPayloads := []map[string]any{
		{"operation": "set_quota", "quotaBytes": "abc"},
		{"operation": "set_quota", "quotaBytes": 1.5},
		{"operation": "set_quota", "quotaBytes": -1},
		{"operation": "set_quota"},
		{"operation": "set_quota", "quotaBytes": 1e30},
	}
	for _, payload := range invalidPayloads {
		recorder := env.doJSON(t, http.MethodPatch, path, payload, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_QUOTA" {
			t.Fatalf("%v 应为 400 INVALID_QUOTA，实际 %d %s", payload, recorder.Code, recorder.Body.String())
		}
	}
	var afterOverride interface{}
	if err := env.app.db.QueryRow(`SELECT quotaBytes FROM "User" WHERE id = ?`, userID).Scan(&afterOverride); err != nil {
		t.Fatalf("读取配额失败: %v", err)
	}
	if fmt.Sprint(beforeOverride) != fmt.Sprint(afterOverride) {
		t.Fatalf("非法配额不应改库：%v → %v", beforeOverride, afterOverride)
	}

	rows := env.auditRowsFor(t, auditActionAdminUserQuotaUpdate)
	if len(rows) != 5 {
		t.Fatalf("应有 5 条配额审计（含两次 no-op），实际 %d", len(rows))
	}
	if !strings.Contains(rows[0].Detail.String, "changed=false") || !strings.Contains(rows[0].Detail.String, "quotaBytes=inherit") {
		t.Fatalf("no-op 审计应记录 changed=false 与 inherit: %q", rows[0].Detail.String)
	}
}

func TestAdminUserSetEmail(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, _ := env.addUser(t, "plain@example.com")
	env.addUser(t, "taken@example.com")

	// 先放一条一次性令牌，改邮箱后必须被清掉。
	if _, err := env.app.db.Exec(`INSERT INTO "AccountToken"
		(id, userId, purpose, tokenHash, email, currentEmail, sessionVersion, expiresAt, createdAt)
		VALUES (?, ?, 'PASSWORD_RESET', ?, 'plain@example.com', 'plain@example.com', 0, ?, ?)`,
		uuid.NewString(), userID, uuid.NewString(), coreNow(), coreNow()); err != nil {
		t.Fatalf("插入测试 AccountToken 失败: %v", err)
	}

	path := fmt.Sprintf("/api/admin/users/%d", userID)
	recorder := env.doJSON(t, http.MethodPatch, path,
		map[string]any{"operation": "set_email", "email": "Renamed@Example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("改邮箱应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	if email := adminTestString(t, body, "email"); email != "renamed@example.com" {
		t.Fatalf("应返回规范化邮箱，实际 %q", email)
	}
	if !adminTestBool(t, body, "changed") {
		t.Fatalf("实际改动时 changed 应为 true: %v", body)
	}
	var tokens int64
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "AccountToken" WHERE userId = ?`, userID).Scan(&tokens); err != nil {
		t.Fatalf("读取 AccountToken 失败: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("改邮箱后该用户的一次性令牌应被清空，实际 %d", tokens)
	}
	row := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminUserEmailUpdate), auditActionAdminUserEmailUpdate)
	if !strings.Contains(row.Detail.String, "previous=plain@example.com") || !strings.Contains(row.Detail.String, "changed=true") {
		t.Fatalf("审计 detail 应记录前后值与 changed: %q", row.Detail.String)
	}

	// 新邮箱能登录，旧邮箱不再能登录。
	if recorder := env.login(t, "renamed@example.com", adminTestUserPassword); recorder.Code != http.StatusOK {
		t.Fatalf("新邮箱应能用原密码登录，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.login(t, "plain@example.com", adminTestUserPassword); recorder.Code != http.StatusBadRequest {
		t.Fatalf("旧邮箱不应再能登录（400 INVALID_CREDENTIALS），实际 %d", recorder.Code)
	}

	// 相同邮箱（忽略大小写）→ changed=false。
	recorder = env.doJSON(t, http.MethodPatch, path,
		map[string]any{"operation": "set_email", "email": "RENAMED@example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK || adminTestBool(t, adminTestJSON(t, recorder), "changed") {
		t.Fatalf("相同邮箱应为 200 changed=false，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 与其他账号冲突 → 409 EMAIL_EXISTS。
	recorder = env.doJSON(t, http.MethodPatch, path,
		map[string]any{"operation": "set_email", "email": "taken@example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusConflict || adminTestErrorCode(t, recorder) != "EMAIL_EXISTS" {
		t.Fatalf("冲突邮箱应为 409 EMAIL_EXISTS，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 非法邮箱 → 400。
	recorder = env.doJSON(t, http.MethodPatch, path,
		map[string]any{"operation": "set_email", "email": "nope"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_EMAIL" {
		t.Fatalf("非法邮箱应为 400 INVALID_EMAIL，实际 %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminUserResetExportCooldown(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, _ := env.addUser(t, "plain@example.com")
	path := fmt.Sprintf("/api/admin/users/%d", userID)

	call := func(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		recorder := env.doJSON(t, http.MethodPatch, path,
			map[string]string{"operation": "reset_export_cooldown"}, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("解除导出限制应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
		body := adminTestJSON(t, recorder)
		cooldown, ok := body["exportCooldown"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 exportCooldown: %v", body)
		}
		return recorder, cooldown
	}

	// 1) 本来就没有冷却 → reset=false 且没有副作用。
	_, cooldown := call(t)
	if adminTestBool(t, cooldown, "reset") {
		t.Fatalf("没有冷却时 reset 必须为 false: %v", cooldown)
	}
	if value, present := cooldown["previousLastExportAt"]; !present || value != nil {
		t.Fatalf("previousLastExportAt 应为 null，实际 %v", value)
	}
	if adminTestBool(t, cooldown, "reservationActive") {
		t.Fatalf("没有预约时 reservationActive 应为 false: %v", cooldown)
	}

	// 2) 存在成功冷却 → reset=true 并清空。
	if _, err := env.app.db.Exec(`UPDATE "User" SET lastExportAt = ? WHERE id = ?`, imagesNow(), userID); err != nil {
		t.Fatalf("写入 lastExportAt 失败: %v", err)
	}
	_, cooldown = call(t)
	if !adminTestBool(t, cooldown, "reset") {
		t.Fatalf("存在冷却时 reset 应为 true: %v", cooldown)
	}
	if value, ok := cooldown["previousLastExportAt"].(string); !ok || value == "" {
		t.Fatalf("previousLastExportAt 应返回被清掉的旧值，实际 %v", cooldown["previousLastExportAt"])
	}
	var lastExport, reservation interface{}
	if err := env.app.db.QueryRow(`SELECT lastExportAt, exportReservationAt FROM "User" WHERE id = ?`, userID).
		Scan(&lastExport, &reservation); err != nil {
		t.Fatalf("读取导出状态失败: %v", err)
	}
	if lastExport != nil {
		t.Fatalf("成功冷却应被清空，实际 %v", lastExport)
	}

	// 3) 租约内的进行中预约绝不能被清除。
	if _, err := env.app.db.Exec(`UPDATE "User" SET exportReservationAt = ?, exportReservationToken = 'tok' WHERE id = ?`,
		imagesNow(), userID); err != nil {
		t.Fatalf("写入预约失败: %v", err)
	}
	_, cooldown = call(t)
	if adminTestBool(t, cooldown, "reset") {
		t.Fatalf("只有进行中预约时 reset 必须为 false: %v", cooldown)
	}
	if !adminTestBool(t, cooldown, "reservationActive") {
		t.Fatalf("租约内预约应报告 reservationActive=true: %v", cooldown)
	}
	if adminTestNumber(t, cooldown, "retryAfterSeconds") <= 0 {
		t.Fatalf("retryAfterSeconds 应为正数: %v", cooldown)
	}
	if value, ok := cooldown["reservationExpiresAt"].(string); !ok || value == "" {
		t.Fatalf("reservationExpiresAt 应为时间戳: %v", cooldown["reservationExpiresAt"])
	}
	var keptToken interface{}
	if err := env.app.db.QueryRow(`SELECT exportReservationToken FROM "User" WHERE id = ?`, userID).Scan(&keptToken); err != nil {
		t.Fatalf("读取预约 token 失败: %v", err)
	}
	if keptToken != "tok" {
		t.Fatalf("进行中的预约不能被清除，实际 token=%v", keptToken)
	}

	// 4) 已过期的预约属于崩溃残留 → 与冷却一起清理，reset=true。
	expired := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond).Format(imagesTimeLayout)
	if _, err := env.app.db.Exec(`UPDATE "User" SET exportReservationAt = ?, exportReservationToken = 'stale' WHERE id = ?`,
		expired, userID); err != nil {
		t.Fatalf("写入过期预约失败: %v", err)
	}
	_, cooldown = call(t)
	if !adminTestBool(t, cooldown, "reset") {
		t.Fatalf("清理崩溃残留预约时 reset 应为 true: %v", cooldown)
	}
	var stale any
	if err := env.app.db.QueryRow(`SELECT exportReservationToken FROM "User" WHERE id = ?`, userID).Scan(&stale); err != nil {
		t.Fatalf("读取预约 token 失败: %v", err)
	}
	if stale != nil {
		t.Fatalf("过期预约应被清除，实际 token=%v", stale)
	}

	rows := env.auditRowsFor(t, auditActionAdminUserExportCooldownReset)
	if len(rows) != 4 {
		t.Fatalf("应有 4 条解除导出限制审计，实际 %d", len(rows))
	}
	if !strings.Contains(rows[0].Detail.String, "changed=false") {
		t.Fatalf("no-op 审计应记录 changed=false: %q", rows[0].Detail.String)
	}
	if !strings.Contains(rows[2].Detail.String, "reservation=kept_active") || !strings.Contains(rows[2].Detail.String, "changed=false") {
		t.Fatalf("进行中预约的审计应记录 kept_active：%q", rows[2].Detail.String)
	}
}

// TestAdminWritesRollBackWhenAuditFails 证明"审计写失败 → 业务写入回滚"。
// 直接删掉 AuditLog 表制造审计写入失败，再逐个验证业务字段没有变化。
func TestAdminWritesRollBackWhenAuditFails(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, _ := env.addUser(t, "plain@example.com")
	imageID := env.addImage(t, userID, "photo.png", adminTestImageOptions{IsVisible: 1})

	if _, err := env.app.db.Exec(`DROP TABLE "AuditLog"`); err != nil {
		t.Fatalf("删除 AuditLog 表失败: %v", err)
	}

	// 改配额：应 500，且配额保持 NULL。
	recorder := env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]any{"operation": "set_quota", "quotaBytes": 5 << 20}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时改配额应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var quota interface{}
	if err := env.app.db.QueryRow(`SELECT quotaBytes FROM "User" WHERE id = ?`, userID).Scan(&quota); err != nil {
		t.Fatalf("读取配额失败: %v", err)
	}
	if quota != nil {
		t.Fatalf("审计失败时配额必须回滚，实际 %v", quota)
	}

	// 禁用账号：应 500，且 disabledAt 仍为 NULL。
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "disable"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时禁用应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if disabled := env.userDisabledAt(t, userID); disabled.Valid {
		t.Fatalf("审计失败时禁用必须回滚，实际 disabledAt=%v", disabled)
	}

	// 隐藏图片：应 500，且 isVisible 不变。
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/"+imageID,
		map[string]string{"operation": "hide"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时隐藏图片应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var visible int
	if err := env.app.db.QueryRow(`SELECT isVisible FROM "Image" WHERE id = ?`, imageID).Scan(&visible); err != nil {
		t.Fatalf("读取图片可见性失败: %v", err)
	}
	if visible != 1 {
		t.Fatalf("审计失败时隐藏图片必须回滚，实际 isVisible=%d", visible)
	}

	// 建号：应 500，且不留下账号。
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/users",
		map[string]string{"email": "another@example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时建号应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var created int64
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "User" WHERE email = 'another@example.com'`).Scan(&created); err != nil {
		t.Fatalf("读取账号数失败: %v", err)
	}
	if created != 0 {
		t.Fatalf("审计失败时建号必须回滚，实际留下 %d 个账号", created)
	}

	// 重置密码：应 500，旧密码仍然有效（说明 passwordHash/sessionVersion 都没改）。
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "reset_password", "password": "another-valid-pass"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时重置密码应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.login(t, "plain@example.com", adminTestUserPassword); recorder.Code != http.StatusOK {
		t.Fatalf("审计失败时重置密码必须回滚，旧密码应仍可登录，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 改邮箱：应 500，邮箱保持原值。
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]any{"operation": "set_email", "email": "moved@example.com"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时改邮箱应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var email string
	if err := env.app.db.QueryRow(`SELECT email FROM "User" WHERE id = ?`, userID).Scan(&email); err != nil {
		t.Fatalf("读取邮箱失败: %v", err)
	}
	if email != "plain@example.com" {
		t.Fatalf("审计失败时改邮箱必须回滚，实际 %q", email)
	}

	// 解除导出冷却：应 500，lastExportAt 保持原值。
	if _, err := env.app.db.Exec(`UPDATE "User" SET lastExportAt = ? WHERE id = ?`, imagesNow(), userID); err != nil {
		t.Fatalf("写入 lastExportAt 失败: %v", err)
	}
	recorder = env.doJSON(t, http.MethodPatch, fmt.Sprintf("/api/admin/users/%d", userID),
		map[string]string{"operation": "reset_export_cooldown"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时解除导出限制应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var lastExport interface{}
	if err := env.app.db.QueryRow(`SELECT lastExportAt FROM "User" WHERE id = ?`, userID).Scan(&lastExport); err != nil {
		t.Fatalf("读取 lastExportAt 失败: %v", err)
	}
	if lastExport == nil {
		t.Fatalf("审计失败时解除导出限制必须回滚")
	}

	// 撤销激活码：应 500，revokedAt 仍为 NULL。
	codeID := env.addActivationCode(t, "ABCD-EFGH-JKMN-PQRS-TVWX", 1, coreNow(), nil, nil, nil)
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/activation-codes/"+codeID,
		map[string]string{"operation": "revoke"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时撤销激活码应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var revokedAt interface{}
	if err := env.app.db.QueryRow(`SELECT revokedAt FROM "ActivationCode" WHERE id = ?`, codeID).Scan(&revokedAt); err != nil {
		t.Fatalf("读取 revokedAt 失败: %v", err)
	}
	if revokedAt != nil {
		t.Fatalf("审计失败时撤销激活码必须回滚，实际 revokedAt=%v", revokedAt)
	}

	// 管理员下架：应 500，deletedAt 仍为 NULL。
	recorder = env.do(t, http.MethodDelete, "/api/admin/images/"+imageID, nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时下架图片应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var deletedAt interface{}
	if err := env.app.db.QueryRow(`SELECT deletedAt FROM "Image" WHERE id = ?`, imageID).Scan(&deletedAt); err != nil {
		t.Fatalf("读取 deletedAt 失败: %v", err)
	}
	if deletedAt != nil {
		t.Fatalf("审计失败时下架图片必须回滚，实际 deletedAt=%v", deletedAt)
	}

	// 生成激活码：应 500，且一行都不留。
	recorder = env.doJSON(t, http.MethodPost, "/api/admin/activation-codes",
		map[string]any{"count": 2}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败时生成激活码应 500，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var codes int64
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "ActivationCode"`).Scan(&codes); err != nil {
		t.Fatalf("读取激活码数失败: %v", err)
	}
	// 上面为"撤销回滚"预置了 1 个激活码；本次生成不应留下任何新行。
	if codes != 1 {
		t.Fatalf("审计失败时生成激活码必须回滚（应只剩预置的 1 个），实际 %d 个", codes)
	}
}
