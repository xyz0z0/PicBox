package server

// 管理端图片接口的回归：列表范围/筛选/搜索/游标分页、hide/show/delete、
// 管理员下架与上传者回收站互不可见，以及每次写入的审计行。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func adminTestCursorWalk(t *testing.T, env *adminTestEnv, adminCookie *http.Cookie, base string, limit int) []string {
	t.Helper()
	collected := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	cursor := ""
	for page := 0; page < 10; page++ {
		target := base + fmt.Sprintf("&limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		recorder := env.do(t, http.MethodGet, target, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("游标分页请求应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
		body := adminTestJSON(t, recorder)
		items := adminTestListItems(t, body, "items")
		for _, item := range items {
			id := adminTestString(t, item, "id")
			if _, dup := seen[id]; dup {
				t.Fatalf("游标分页出现重复图片: %s", id)
			}
			seen[id] = struct{}{}
			collected = append(collected, id)
		}
		next, ok := body["nextCursor"].(string)
		if !ok || next == "" {
			return collected
		}
		cursor = next
	}
	t.Fatalf("游标分页未在 10 页内结束，已收集 %d 条", len(collected))
	return collected
}

func TestAdminImagesListFilterSearchAndCursor(t *testing.T) {
	env := adminTestNewApp(t, Config{BaseURL: "https://pic.example.com"})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, _ := env.addUser(t, "owner@example.com")

	sameSecond := "2026-01-02T03:04:05.000Z"
	wanted := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		wanted = append(wanted, env.addImage(t, userID, fmt.Sprintf("shot-%d.png", index),
			adminTestImageOptions{CreatedAt: sameSecond, IsVisible: 1}))
	}
	// 已软删（上传者回收站）与管理员下架的图片都不应出现在管理端列表里。
	trashedAt := sameSecond
	env.addImage(t, userID, "trashed.png", adminTestImageOptions{CreatedAt: sameSecond, DeletedAt: &trashedAt})
	env.addImage(t, userID, "banned.png", adminTestImageOptions{CreatedAt: sameSecond, DeletedAt: &trashedAt, DeletedByAdmin: 1})
	hiddenID := env.addImage(t, userID, "hidden-asset.png", adminTestImageOptions{CreatedAt: sameSecond, IsVisible: 0})

	// 同一秒的 6 张可见图片：游标必须覆盖全部且不重复。
	collected := adminTestCursorWalk(t, env, adminCookie, "/api/admin/images?", 2)
	if len(collected) != 6 {
		t.Fatalf("应遍历到 6 张未删除图片，实际 %d: %v", len(collected), collected)
	}
	collectedSet := make(map[string]struct{}, len(collected))
	for _, id := range collected {
		collectedSet[id] = struct{}{}
	}
	for _, id := range wanted {
		if _, ok := collectedSet[id]; !ok {
			t.Fatalf("游标分页丢失图片 %s", id)
		}
	}

	// 列表字段与 URL 前缀。
	recorder := env.do(t, http.MethodGet, "/api/admin/images?limit=100", nil, adminTestCookieAuth(adminCookie))
	body := adminTestJSON(t, recorder)
	items := adminTestListItems(t, body, "items")
	if len(items) != 6 {
		t.Fatalf("limit=100 应返回 6 行，实际 %d", len(items))
	}
	first := items[0]
	if id := adminTestString(t, first, "id"); id == "" {
		t.Fatalf("缺少 id: %v", first)
	}
	uploader, ok := first["uploader"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 uploader: %v", first)
	}
	if email := adminTestString(t, uploader, "email"); email != "owner@example.com" {
		t.Fatalf("uploader.email 应为上传者，实际 %q", email)
	}
	if url := adminTestString(t, first, "url"); !strings.HasPrefix(url, "https://pic.example.com/api/images/") {
		t.Fatalf("url 应使用配置的 BaseURL，实际 %q", url)
	}
	if page := adminTestString(t, first, "pageUrl"); !strings.HasPrefix(page, "https://pic.example.com/images/") {
		t.Fatalf("pageUrl 前缀不正确: %q", page)
	}
	if thumb := adminTestString(t, first, "thumbUrl"); !strings.HasSuffix(thumb, "?variant=thumb") {
		t.Fatalf("thumbUrl 应指向缩略图，实际 %q", thumb)
	}

	// 可见性筛选。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?visibility=hidden&limit=100", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	hiddenItems := adminTestListItems(t, body, "items")
	if len(hiddenItems) != 1 || adminTestString(t, hiddenItems[0], "id") != hiddenID {
		t.Fatalf("visibility=hidden 应只返回隐藏图 %s，实际 %v", hiddenID, hiddenItems)
	}
	if adminTestBool(t, hiddenItems[0], "isVisible") {
		t.Fatalf("隐藏图 isVisible 应为 false")
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/images?visibility=visible&limit=100", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 5 {
		t.Fatalf("visibility=visible 应为 5 行，实际 %d", len(rows))
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/images?visibility=bogus", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_VISIBILITY" {
		t.Fatalf("非法 visibility 应为 400 INVALID_VISIBILITY，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 搜索：文件名、上传者邮箱、上传者 username。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?q=shot-3", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 1 {
		t.Fatalf("按文件名搜索应命中 1，实际 %d", len(rows))
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/images?q=owner@example.com", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 6 {
		t.Fatalf("按上传者邮箱搜索应命中 6，实际 %d", len(rows))
	}
	var username string
	if err := env.app.db.QueryRow(`SELECT username FROM "User" WHERE id = ?`, userID).Scan(&username); err != nil {
		t.Fatalf("读取 username 失败: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/images?q="+username, nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 6 {
		t.Fatalf("按上传者 username 搜索应命中 6，实际 %d", len(rows))
	}
	// LIKE 通配符必须被转义。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?q=%25", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 0 {
		t.Fatalf("q=%% 不应命中任何图片，实际 %d", len(rows))
	}

	// 非法游标按首页处理，不报错也不丢数据。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?cursor=not-a-date", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("非法游标应降级为首页，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if rows := adminTestListItems(t, adminTestJSON(t, recorder), "items"); len(rows) != 6 {
		t.Fatalf("非法游标应返回首页全部 6 行，实际 %d", len(rows))
	}

	// 兼容旧的纯时间戳游标（只按 createdAt < 过滤）。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?cursor=2026-01-03T00:00:00.000Z", nil, adminTestCookieAuth(adminCookie))
	body = adminTestJSON(t, recorder)
	if rows := adminTestListItems(t, body, "items"); len(rows) != 6 {
		t.Fatalf("旧游标格式应继续可用，实际 %d 行", len(rows))
	}
}

func TestAdminImageVisibilityDeleteAndTrashBoundary(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	userID, userCookie := env.addUser(t, "owner@example.com")
	imageID := env.addImage(t, userID, "photo.png", adminTestImageOptions{IsVisible: 1})

	// hide → show，各自写一条审计。
	recorder := env.doJSON(t, http.MethodPatch, "/api/admin/images/"+imageID,
		map[string]string{"operation": "hide"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("隐藏图片应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	body := adminTestJSON(t, recorder)
	image, _ := body["image"].(map[string]any)
	if adminTestBool(t, image, "isVisible") {
		t.Fatalf("隐藏后 isVisible 应为 false: %v", image)
	}
	hideAudit := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminImageHide), auditActionAdminImageHide)
	if hideAudit.Detail.String != "imageId="+imageID {
		t.Fatalf("隐藏审计 detail 应为 imageId=...，实际 %q", hideAudit.Detail.String)
	}

	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/"+imageID,
		map[string]string{"operation": "show"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("显示图片应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	image, _ = adminTestJSON(t, recorder)["image"].(map[string]any)
	if !adminTestBool(t, image, "isVisible") {
		t.Fatalf("显示后 isVisible 应为 true: %v", image)
	}
	adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminImageShow), auditActionAdminImageShow)

	// 非法 operation、未知 id。
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/"+imageID,
		map[string]string{"operation": "delete"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusBadRequest || adminTestErrorCode(t, recorder) != "INVALID_OPERATION" {
		t.Fatalf("非法 operation 应为 400 INVALID_OPERATION，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/missing",
		map[string]string{"operation": "hide"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusNotFound || adminTestErrorCode(t, recorder) != "NOT_FOUND" {
		t.Fatalf("未知图片应为 404 NOT_FOUND，实际 %d %s", recorder.Code, recorder.Body.String())
	}

	// 上传者先放进自己回收站的图片：管理员仍可隐藏/删除（原版语义）。
	softDeleted := imagesNow()
	userTrashedID := env.addImage(t, userID, "user-trashed.png", adminTestImageOptions{DeletedAt: &softDeleted, IsVisible: 1})
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/"+userTrashedID,
		map[string]string{"operation": "hide"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传者已软删的图片仍应允许管理员隐藏，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 管理员下架：deletedByAdmin=1，上传者回收站与个人列表都不出现。
	recorder = env.do(t, http.MethodDelete, "/api/admin/images/"+imageID, nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("管理员下架应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	var deletedAt, deletedByAdmin interface{}
	if err := env.app.db.QueryRow(`SELECT deletedAt, deletedByAdmin FROM "Image" WHERE id = ?`, imageID).
		Scan(&deletedAt, &deletedByAdmin); err != nil {
		t.Fatalf("读取图片状态失败: %v", err)
	}
	if deletedAt == nil {
		t.Fatalf("管理员下架必须设置 deletedAt")
	}
	if fmt.Sprint(deletedByAdmin) != "1" {
		t.Fatalf("管理员下架必须设置 deletedByAdmin=1，实际 %v", deletedByAdmin)
	}
	deleteAudit := adminTestMustOne(t, env.auditRowsFor(t, auditActionAdminImageDelete), auditActionAdminImageDelete)
	if deleteAudit.Detail.String != "imageId="+imageID {
		t.Fatalf("下架审计 detail 应为 imageId=...，实际 %q", deleteAudit.Detail.String)
	}

	// 重复下架 → 404（原版：已被管理员下架即视为不存在）。
	recorder = env.do(t, http.MethodDelete, "/api/admin/images/"+imageID, nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("重复下架应为 404，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = env.doJSON(t, http.MethodPatch, "/api/admin/images/"+imageID,
		map[string]string{"operation": "show"}, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("已下架图片的 PATCH 应为 404，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 上传者视角：个人列表与回收站都不包含管理员下架的图片。
	recorder = env.do(t, http.MethodGet, "/api/images?mine=true", nil, adminTestCookieAuth(userCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("个人列表应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, item := range adminTestListItems(t, adminTestJSON(t, recorder), "items") {
		if adminTestString(t, item, "id") == imageID {
			t.Fatalf("管理员下架的图片不应出现在个人列表")
		}
	}
	recorder = env.do(t, http.MethodGet, "/api/images?mine=true&trashed=true", nil, adminTestCookieAuth(userCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("回收站列表应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	trashItems := adminTestListItems(t, adminTestJSON(t, recorder), "items")
	foundUserTrashed := false
	for _, item := range trashItems {
		id := adminTestString(t, item, "id")
		if id == imageID {
			t.Fatalf("管理员下架的图片不应出现在个人回收站")
		}
		if id == userTrashedID {
			foundUserTrashed = true
		}
	}
	if !foundUserTrashed {
		t.Fatalf("上传者自己软删的图片应仍在回收站")
	}

	// 管理端列表也不再包含已下架图片。
	recorder = env.do(t, http.MethodGet, "/api/admin/images?limit=100", nil, adminTestCookieAuth(adminCookie))
	for _, item := range adminTestListItems(t, adminTestJSON(t, recorder), "items") {
		if adminTestString(t, item, "id") == imageID {
			t.Fatalf("已下架图片不应出现在管理端列表")
		}
	}
}
