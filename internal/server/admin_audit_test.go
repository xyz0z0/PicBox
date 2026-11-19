package server

// 审计日志查询接口的回归：14 个动作枚举、非法动作、搜索（详情/操作者/目标）、
// 同一秒多行的游标分页边界，以及响应字段形状。

import (
	"fmt"
	"net/http"
	"testing"
)

func adminTestInsertAudit(t *testing.T, env *adminTestEnv, actorID int64, action string, targetID any, detail any, createdAt string) {
	t.Helper()
	if _, err := env.app.db.Exec(`INSERT INTO "AuditLog" (actorId, action, targetUserId, detail, createdAt)
		VALUES (?, ?, ?, ?, ?)`, actorID, action, targetID, detail, createdAt); err != nil {
		t.Fatalf("插入测试审计行失败: %v", err)
	}
}

func adminTestWalkAudit(t *testing.T, env *adminTestEnv, adminCookie *http.Cookie, base string, limit int) []map[string]any {
	t.Helper()
	collected := make([]map[string]any, 0, 16)
	seen := make(map[int64]struct{}, 16)
	cursor := ""
	for page := 0; page < 20; page++ {
		target := base + fmt.Sprintf("&limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		recorder := env.do(t, http.MethodGet, target, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("审计日志请求应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
		body := adminTestJSON(t, recorder)
		for _, item := range adminTestListItems(t, body, "logs") {
			id := adminTestNumber(t, item, "id")
			if _, dup := seen[id]; dup {
				t.Fatalf("审计游标分页出现重复行 id=%d", id)
			}
			seen[id] = struct{}{}
			collected = append(collected, item)
		}
		next, ok := body["nextCursor"].(string)
		if !ok || next == "" {
			return collected
		}
		cursor = next
	}
	t.Fatalf("审计游标分页未在 20 页内结束，已收集 %d 行", len(collected))
	return collected
}

func TestAdminAuditLogsActionsAndShape(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	targetID, _ := env.addUser(t, "target@example.com")

	actions := auditActionValues()
	if len(actions) != 14 {
		t.Fatalf("审计动作必须是 14 个枚举值，实际 %d", len(actions))
	}
	createdAt := coreNow()
	for index, action := range actions {
		var target any
		detail := "seq=" + action
		if index%2 == 0 {
			target = targetID
		}
		adminTestInsertAudit(t, env, 1, action, target, detail, createdAt)
	}

	// action=all 返回全部 14 条（同秒多行，游标必须覆盖全部）。
	all := adminTestWalkAudit(t, env, adminCookie, "/api/admin/audit-logs?", 3)
	if len(all) != 14 {
		t.Fatalf("应遍历到 14 条审计，实际 %d", len(all))
	}

	// 每个动作都能单独筛选，且只返回该动作。
	for index, action := range actions {
		recorder := env.do(t, http.MethodGet, "/api/admin/audit-logs?action="+action, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("action=%s 应为 200，实际 %d: %s", action, recorder.Code, recorder.Body.String())
		}
		logs := adminTestListItems(t, adminTestJSON(t, recorder), "logs")
		if len(logs) != 1 {
			t.Fatalf("action=%s 应只返回 1 条，实际 %d", action, len(logs))
		}
		if value := adminTestString(t, logs[0], "action"); value != action {
			t.Fatalf("action=%s 返回了 %q", action, value)
		}
		if detail := adminTestString(t, logs[0], "detail"); detail != "seq="+action {
			t.Fatalf("detail 不匹配: %q", detail)
		}
		actor, ok := logs[0]["actor"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 actor: %v", logs[0])
		}
		if email := adminTestString(t, actor, "email"); email != "root@example.com" {
			t.Fatalf("actor.email 应为管理员邮箱，实际 %q", email)
		}
		if actorID := adminTestNumber(t, actor, "id"); actorID != 1 {
			t.Fatalf("actor.id 应为 1，实际 %d", actorID)
		}
		if index%2 == 0 {
			target, ok := logs[0]["targetUser"].(map[string]any)
			if !ok {
				t.Fatalf("action=%s 应带 targetUser: %v", action, logs[0])
			}
			if id := adminTestNumber(t, target, "id"); id != targetID {
				t.Fatalf("targetUser.id 应为 %d，实际 %d", targetID, id)
			}
		} else if logs[0]["targetUser"] != nil {
			// 没有目标的动作必须返回 null，而不是空对象。
			t.Fatalf("action=%s 的 targetUser 应为 null: %v", action, logs[0]["targetUser"])
		}
	}

	// 非法动作 → 400 INVALID_ACTION。
	recorder := env.do(t, http.MethodGet, "/api/admin/audit-logs?action=ADMIN_TELEPORT", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_ACTION" {
		t.Fatalf("非法 action 应为 400 INVALID_ACTION，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// detail 为 NULL 时返回 JSON null。
	adminTestInsertAudit(t, env, 1, auditActionAdminLogin, nil, nil, coreNow())
	recorder = env.do(t, http.MethodGet, "/api/admin/audit-logs?action=ADMIN_LOGIN&limit=100", nil, adminTestCookieAuth(adminCookie))
	logs := adminTestListItems(t, adminTestJSON(t, recorder), "logs")
	if len(logs) != 2 {
		t.Fatalf("ADMIN_LOGIN 应为 2 条，实际 %d", len(logs))
	}
	foundNullDetail := false
	for _, item := range logs {
		if item["detail"] == nil {
			foundNullDetail = true
		}
	}
	if !foundNullDetail {
		t.Fatalf("空 detail 必须返回 null: %v", logs)
	}
}

func TestAdminAuditLogsSearchAndPaginationBoundary(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	targetID, _ := env.addUser(t, "target@example.com")

	createdAt := coreNow()
	adminTestInsertAudit(t, env, 1, auditActionAdminUserQuotaUpdate, targetID, "quotaBytes=1048576; changed=true", createdAt)
	adminTestInsertAudit(t, env, 1, auditActionAdminUserEmailUpdate, targetID, "email=target@example.com", createdAt)
	adminTestInsertAudit(t, env, 1, auditActionAdminImageHide, nil, "imageId=abc", createdAt)
	adminTestInsertAudit(t, env, 1, auditActionAdminActivationCodeCreate, nil, "count=2; note=beijing", createdAt)

	// 注意：响应里没有 total 字段（与原版一致），用长度断言搜索命中数。
	search := func(t *testing.T, query string) []map[string]any {
		t.Helper()
		recorder := env.do(t, http.MethodGet, "/api/admin/audit-logs?limit=100&"+query, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("查询 %s 应为 200，实际 %d: %s", query, recorder.Code, recorder.Body.String())
		}
		return adminTestListItems(t, adminTestJSON(t, recorder), "logs")
	}
	if rows := search(t, "q=quotaBytes"); len(rows) != 1 {
		t.Fatalf("按 detail 搜索应命中 1，实际 %d", len(rows))
	}
	if rows := search(t, "q=beijing"); len(rows) != 1 {
		t.Fatalf("按备注片段搜索应命中 1，实际 %d", len(rows))
	}
	if rows := search(t, "q=root@example.com"); len(rows) != 4 {
		t.Fatalf("按操作者邮箱搜索应命中 4，实际 %d", len(rows))
	}
	if rows := search(t, "q=target@example.com"); len(rows) != 2 {
		t.Fatalf("按目标账号邮箱搜索应命中 2，实际 %d", len(rows))
	}
	var targetUsername string
	if err := env.app.db.QueryRow(`SELECT username FROM "User" WHERE id = ?`, targetID).Scan(&targetUsername); err != nil {
		t.Fatalf("读取 username 失败: %v", err)
	}
	if rows := search(t, "q="+targetUsername); len(rows) != 2 {
		t.Fatalf("按目标账号 username 搜索应命中 2，实际 %d", len(rows))
	}
	if rows := search(t, "q=%25"); len(rows) != 0 {
		t.Fatalf("q=%% 应按字面量匹配（0 行），实际 %d", len(rows))
	}
	if rows := search(t, "action=ADMIN_IMAGE_HIDE&q=abc"); len(rows) != 1 {
		t.Fatalf("action 与 q 应可组合，实际 %d", len(rows))
	}

	// 同一秒 4 行 + limit=2：游标必须覆盖全部且不重复（含 = 边界的 id 兜底）。
	collected := adminTestWalkAudit(t, env, adminCookie, "/api/admin/audit-logs?", 2)
	if len(collected) != 4 {
		t.Fatalf("同秒分页应覆盖全部 4 行，实际 %d", len(collected))
	}
	ids := make([]int64, 0, 4)
	for _, item := range collected {
		ids = append(ids, adminTestNumber(t, item, "id"))
	}
	for index := 1; index < len(ids); index++ {
		if ids[index] >= ids[index-1] {
			t.Fatalf("审计日志必须按 createdAt desc, id desc 排序，实际 %v", ids)
		}
	}

	// 非法游标按首页处理。
	recorder := env.do(t, http.MethodGet, "/api/admin/audit-logs?cursor=garbage", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("非法游标应降级为首页，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if rows := adminTestListItems(t, adminTestJSON(t, recorder), "logs"); len(rows) != 4 {
		t.Fatalf("非法游标应返回首页，实际 %d 行", len(rows))
	}
}
