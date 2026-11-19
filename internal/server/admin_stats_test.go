package server

// 管理端总览统计的回归：卡片计数、存储口径（含回收站）、今日上传、配额告急数量与
// quotaTop 的排序/字段。全部使用真实 SQLite，不依赖任何占位数据。

import (
	"net/http"
	"testing"
	"time"
)

func TestAdminStatsOverview(t *testing.T) {
	env := adminTestNewApp(t, Config{UserQuotaBytes: 10 << 20})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	fullID, _ := env.addUser(t, "full@example.com")
	alertID, _ := env.addUser(t, "alert@example.com")
	idleID, _ := env.addUser(t, "idle@example.com")

	// full：正好用满 10MiB（其中 2MiB 在回收站）；alert：4MiB/5MiB = 0.8；
	// idle：继承全局 10MiB，只有一张两天前的 1MiB 图片（不计入今日上传）。
	if _, err := env.app.db.Exec(`UPDATE "User" SET quotaBytes = ? WHERE id = ?`, 10<<20, fullID); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	if _, err := env.app.db.Exec(`UPDATE "User" SET quotaBytes = ? WHERE id = ?`, 5<<20, alertID); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	if _, err := env.app.db.Exec(`UPDATE "User" SET disabledAt = ? WHERE id = ?`, coreNow(), idleID); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}

	trashedAt := imagesNow()
	env.addImage(t, fullID, "full-a.png", adminTestImageOptions{Size: 8 << 20})
	env.addImage(t, fullID, "full-b.png", adminTestImageOptions{Size: 2 << 20, DeletedAt: &trashedAt})
	env.addImage(t, alertID, "alert.png", adminTestImageOptions{Size: 4 << 20})
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond).Format(imagesTimeLayout)
	env.addImage(t, idleID, "old.png", adminTestImageOptions{Size: 1 << 20, CreatedAt: old})

	recorder := env.do(t, http.MethodGet, "/api/admin/stats", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stats 应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)

	if value := adminTestNumber(t, body, "userCount"); value != 4 {
		t.Fatalf("userCount 应为 4，实际 %d", value)
	}
	if value := adminTestNumber(t, body, "adminCount"); value != 1 {
		t.Fatalf("adminCount 应为 1，实际 %d", value)
	}
	if value := adminTestNumber(t, body, "disabledCount"); value != 1 {
		t.Fatalf("disabledCount 应为 1，实际 %d", value)
	}
	// 4 张图片中 1 张在回收站：imageCount 只算未删除的 3 张。
	if value := adminTestNumber(t, body, "imageCount"); value != 3 {
		t.Fatalf("imageCount 应为 3（不含回收站），实际 %d", value)
	}
	// 存储口径包含回收站：8 + 2 + 4 + 1 = 15MiB。
	if value := adminTestNumber(t, body, "storageUsedBytes"); value != 15<<20 {
		t.Fatalf("storageUsedBytes 应为 15MiB，实际 %d", value)
	}
	// 今日上传按本地 0 点：两天前那张不计入。
	if value := adminTestNumber(t, body, "todayUploadCount"); value != 3 {
		t.Fatalf("todayUploadCount 应为 3，实际 %d", value)
	}
	if value := adminTestNumber(t, body, "quotaAlertCount"); value != 2 {
		t.Fatalf("quotaAlertCount 应为 2（100%% 与 80%%），实际 %d", value)
	}

	top := adminTestListItems(t, body, "quotaTop")
	if len(top) != 2 {
		t.Fatalf("quotaTop 应为 2 行，实际 %d: %v", len(top), top)
	}
	if id := adminTestNumber(t, top[0], "id"); id != fullID {
		t.Fatalf("quotaTop 首行应为用量最高的账号 %d，实际 %d", fullID, id)
	}
	if label := adminTestString(t, top[0], "label"); label != "full@example.com" {
		t.Fatalf("quotaTop label 应为账号邮箱，实际 %q", label)
	}
	if used := adminTestNumber(t, top[0], "usedBytes"); used != 10<<20 {
		t.Fatalf("配额告急行的 usedBytes 应包含回收站，实际 %d", used)
	}
	if quota := adminTestNumber(t, top[0], "quotaBytes"); quota != 10<<20 {
		t.Fatalf("quotaBytes 应为生效配额 10MiB，实际 %d", quota)
	}
	if ratio, _ := top[0]["ratio"].(float64); ratio != 1 {
		t.Fatalf("满额账号 ratio 应为 1，实际 %v", top[0]["ratio"])
	}
	if !adminTestBool(t, top[0], "exceeded") {
		t.Fatalf("满额账号应标记 exceeded: %v", top[0])
	}
	if id := adminTestNumber(t, top[1], "id"); id != alertID {
		t.Fatalf("quotaTop 第二行应为 80%% 账号 %d，实际 %d", alertID, id)
	}
	if ratio, _ := top[1]["ratio"].(float64); ratio < 0.799 || ratio > 0.801 {
		t.Fatalf("80%% 账号 ratio 应约等于 0.8，实际 %v", top[1]["ratio"])
	}
	if adminTestBool(t, top[1], "exceeded") {
		t.Fatalf("未超过配额的账号不应 exceeded: %v", top[1])
	}
}

func TestAdminStatsQuotaTopLimitAndTieBreak(t *testing.T) {
	env := adminTestNewApp(t, Config{UserQuotaBytes: 100})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	// 6 个账号都达到 100% 告急：quotaTop 只取前 5 个，且相同 ratio 按 id 升序稳定。
	ids := make([]int64, 0, 6)
	for index := 0; index < 6; index++ {
		id, _ := env.addUser(t, "quota-"+string(rune('a'+index))+"@example.com")
		ids = append(ids, id)
		if _, err := env.app.db.Exec(`UPDATE "User" SET quotaBytes = 100 WHERE id = ?`, id); err != nil {
			t.Fatalf("设置配额失败: %v", err)
		}
		env.addImage(t, id, "img.png", adminTestImageOptions{Size: 100})
	}

	recorder := env.do(t, http.MethodGet, "/api/admin/stats", nil, adminTestCookieAuth(adminCookie))
	body := adminTestJSON(t, recorder)
	if value := adminTestNumber(t, body, "quotaAlertCount"); value != 6 {
		t.Fatalf("quotaAlertCount 应统计全部 6 个告急账号，实际 %d", value)
	}
	top := adminTestListItems(t, body, "quotaTop")
	if len(top) != adminQuotaTopLimit {
		t.Fatalf("quotaTop 最多 %d 行，实际 %d", adminQuotaTopLimit, len(top))
	}
	for index := range top {
		if id := adminTestNumber(t, top[index], "id"); id != ids[index] {
			t.Fatalf("相同 ratio 应按 id 升序，第 %d 行为 %d（期望 %d）", index, id, ids[index])
		}
	}
}
