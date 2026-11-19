package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers for the trash/permanent-delete contract ----

func imgTrashTestDo(t *testing.T, env *imgTestEnv, method, target, origin string, auth imgTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if auth.cookie != nil {
		request.AddCookie(auth.cookie)
	}
	if auth.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+auth.bearer)
	}
	recorder := httptest.NewRecorder()
	env.handler.ServeHTTP(recorder, request)
	return recorder
}

func imgTrashTestRestore(t *testing.T, env *imgTestEnv, auth imgTestAuth, imageID string) *httptest.ResponseRecorder {
	t.Helper()
	return imgTrashTestDo(t, env, http.MethodPost, "/api/images/"+imageID+"/restore", "", auth)
}

func imgTrashTestPurge(t *testing.T, env *imgTestEnv, auth imgTestAuth, imageID string) *httptest.ResponseRecorder {
	t.Helper()
	return imgTrashTestDo(t, env, http.MethodDelete, "/api/images/"+imageID+"?permanent=true", "", auth)
}

// imgTrashTestRow returns the persisted state of one image row.
func imgTrashTestRow(t *testing.T, env *imgTestEnv, imageID string) (exists bool, deletedAt *string, isVisible int, updatedAt string) {
	t.Helper()
	err := env.app.db.QueryRow(`SELECT deletedAt, isVisible, updatedAt FROM "Image" WHERE id = ?`, imageID).
		Scan(&deletedAt, &isVisible, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, 0, ""
	}
	if err != nil {
		t.Fatalf("读取图片状态失败: %v", err)
	}
	return true, deletedAt, isVisible, updatedAt
}

func imgTrashTestTags(t *testing.T, env *imgTestEnv, imageID string) []string {
	t.Helper()
	rows, err := env.app.db.Query(`SELECT name FROM "ImageTag" WHERE imageId = ? ORDER BY name`, imageID)
	if err != nil {
		t.Fatalf("读取标签失败: %v", err)
	}
	defer rows.Close()
	names := make([]string, 0, 2)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描标签失败: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历标签失败: %v", err)
	}
	return names
}

// imgTrashTestPaths resolves the stored files of one image through the same guard the
// handlers use, so the test asserts on the real on-disk locations.
func imgTrashTestPaths(t *testing.T, env *imgTestEnv, imageID string) (originalAbs, thumbAbs string) {
	t.Helper()
	originalRel, thumbRel := imgTrashTestRelativePaths(t, env, imageID)
	originalAbs, err := env.app.imagesSafePath(originalRel)
	if err != nil {
		t.Fatalf("解析原图路径失败: %v", err)
	}
	thumbAbs, err = env.app.imagesSafePath(thumbRel)
	if err != nil {
		t.Fatalf("解析缩略图路径失败: %v", err)
	}
	return originalAbs, thumbAbs
}

func imgTrashTestRelativePaths(t *testing.T, env *imgTestEnv, imageID string) (originalRel, thumbRel string) {
	t.Helper()
	if err := env.app.db.QueryRow(`SELECT originalPath, thumbPath FROM "Image" WHERE id = ?`, imageID).
		Scan(&originalRel, &thumbRel); err != nil {
		t.Fatalf("读取存储路径失败: %v", err)
	}
	return originalRel, thumbRel
}

func imgTrashTestUsedBytes(t *testing.T, env *imgTestEnv, userID int64) int64 {
	t.Helper()
	used, err := imagesUsedBytes(context.Background(), env.app.db, userID)
	if err != nil {
		t.Fatalf("统计已用配额失败: %v", err)
	}
	return used
}

func imgTrashTestReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return data
}

func imgTrashTestFileMissing(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return false
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("检查 %s 失败: %v", path, err)
	}
	return true
}

// imgTrashTestLeftovers lists abandoned purge quarantine directories under data/tmp.
func imgTrashTestLeftovers(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, imagesTempDirName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("读取暂存目录失败: %v", err)
	}
	leftovers := make([]string, 0, 2)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), imagesPurgeQuarantinePrefix) {
			leftovers = append(leftovers, entry.Name())
		}
	}
	sort.Strings(leftovers)
	return leftovers
}

func imgTrashTestItems(t *testing.T, recorder *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	body := imgTestJSON(t, recorder)
	rawItems, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 不是数组: %s", recorder.Body.String())
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("item 不是对象: %v", rawItem)
		}
		items = append(items, item)
	}
	return items
}

func imgTrashTestTotal(t *testing.T, recorder *httptest.ResponseRecorder) int64 {
	t.Helper()
	total, _ := imgTestJSON(t, recorder)["total"].(float64)
	return int64(total)
}

func imgTrashTestListIDs(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	ids := imgTestListIDs(t, recorder)
	sort.Strings(ids)
	return ids
}

// imgTrashTestUpload uploads a fixture and fails the test if the upload is rejected.
func imgTrashTestUpload(t *testing.T, env *imgTestEnv, auth imgTestAuth, name string, tags []string) (string, []byte) {
	t.Helper()
	data := imgTestPNG(t, 24, 18)
	recorder, body := env.imgTestUploadBytes(t, auth, name, data, tags)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传 %s 失败: status=%d body=%s", name, recorder.Code, recorder.Body.String())
	}
	return imgTestMustImageID(t, body), data
}

func imgTrashTestContains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ---- recycle bin list ----

func TestImagesTrashListBoundaries(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	ownerCookie := env.registerAdmin(t, "trash-list@example.com", "password123")
	ownerAuth := imgTestCookieAuth(ownerCookie)
	_, otherCookie := env.addUser(t, "trash-other@example.com")
	otherAuth := imgTestCookieAuth(otherCookie)
	keyAuth := imgTestBearerAuth(env.addAPIKey(t, 1))

	activeID, _ := imgTrashTestUpload(t, env, ownerAuth, "active.png", []string{"keep"})
	trashedB, _ := imgTrashTestUpload(t, env, ownerAuth, "trashed-b.png", []string{"blue", "green"})
	imgTestSetHidden(t, env, ownerAuth, trashedB, false)
	time.Sleep(2 * time.Millisecond)
	trashedC, _ := imgTrashTestUpload(t, env, ownerAuth, "trashed-c.png", nil)
	time.Sleep(2 * time.Millisecond)
	imgTestTrash(t, env, ownerAuth, trashedB)
	imgTestTrash(t, env, ownerAuth, trashedC)

	adminRemoved, _ := imgTrashTestUpload(t, env, ownerAuth, "admin-removed.png", nil)
	imgTestTrash(t, env, ownerAuth, adminRemoved)
	if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedByAdmin = 1 WHERE id = ?`, adminRemoved); err != nil {
		t.Fatalf("标记管理员下架失败: %v", err)
	}
	otherTrashed, _ := imgTrashTestUpload(t, env, otherAuth, "other.png", []string{"other"})
	imgTestTrash(t, env, otherAuth, otherTrashed)

	t.Run("pagination-matches-gallery-shape", func(t *testing.T) {
		first := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&limit=1", "", ownerAuth)
		if first.Code != http.StatusOK {
			t.Fatalf("回收站列表 status = %d，body=%s", first.Code, first.Body.String())
		}
		if total := imgTrashTestTotal(t, first); total != 2 {
			t.Fatalf("total = %d，期望 2（只含本人软删除，不含管理员下架）", total)
		}
		items := imgTrashTestItems(t, first)
		if len(items) != 1 {
			t.Fatalf("第一页条数 = %d，期望 1", len(items))
		}
		if items[0]["id"] != trashedC {
			t.Fatalf("newest 顺序错误: %v", items[0]["id"])
		}
		if items[0]["deletedAt"] == nil || items[0]["deletedAt"] == "" {
			t.Fatalf("回收站条目缺少 deletedAt: %v", items[0])
		}
		if _, ok := items[0]["url"].(string); !ok {
			t.Fatalf("回收站条目缺少 url: %v", items[0])
		}
		cursor, _ := imgTestJSON(t, first)["nextCursor"].(string)
		if cursor == "" {
			t.Fatalf("第一页应返回 nextCursor: %s", first.Body.String())
		}
		second := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&limit=1&cursor="+cursor, "", ownerAuth)
		secondItems := imgTrashTestItems(t, second)
		if len(secondItems) != 1 || secondItems[0]["id"] != trashedB {
			t.Fatalf("第二页错误: %v", secondItems)
		}
		// A full page always carries a cursor (same contract as the gallery); the page
		// after the last record must be empty.
		lastCursor, _ := imgTestJSON(t, second)["nextCursor"].(string)
		if lastCursor == "" {
			t.Fatalf("满页应返回 nextCursor: %s", second.Body.String())
		}
		third := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&limit=1&cursor="+lastCursor, "", ownerAuth)
		if items := imgTrashTestItems(t, third); len(items) != 0 {
			t.Fatalf("回收站第三页应为空: %v", items)
		}
	})

	t.Run("trash-item-keeps-tags-and-visibility", func(t *testing.T) {
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&query=trashed-b", "", ownerAuth)
		items := imgTrashTestItems(t, recorder)
		if len(items) != 1 || items[0]["id"] != trashedB {
			t.Fatalf("回收站过滤结果错误: %v", items)
		}
		tags, _ := items[0]["tags"].([]any)
		if len(tags) != 2 || tags[0] != "blue" || tags[1] != "green" {
			t.Fatalf("回收站条目标签错误: %v", items[0]["tags"])
		}
		if items[0]["isVisible"] != false {
			t.Fatalf("回收站条目应保留隐藏状态: %v", items[0]["isVisible"])
		}
	})

	t.Run("active-list-excludes-trash-and-admin-removals", func(t *testing.T) {
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true", "", ownerAuth)
		ids := imgTrashTestListIDs(t, recorder)
		if len(ids) != 1 || ids[0] != activeID {
			t.Fatalf("个人图库应只剩未删除图片: %v", ids)
		}
	})

	t.Run("trash-is-per-account", func(t *testing.T) {
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true", "", otherAuth)
		if total := imgTrashTestTotal(t, recorder); total != 1 {
			t.Fatalf("其他账号回收站 total = %d，期望 1（只有自己的记录）", total)
		}
		if ids := imgTrashTestListIDs(t, recorder); len(ids) != 1 || ids[0] != otherTrashed {
			t.Fatalf("其他账号回收站串号: %v", ids)
		}
	})

	t.Run("cookie-only", func(t *testing.T) {
		anonymous := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true", "", imgTestAuth{})
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("匿名回收站 status = %d，期望 401", anonymous.Code)
		}
		withKey := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true", "", keyAuth)
		if withKey.Code != http.StatusUnauthorized {
			t.Fatalf("API Key 回收站 status = %d，期望 401", withKey.Code)
		}
		if code := imgTestErrorCode(t, withKey); code != "UNAUTHORIZED" {
			t.Fatalf("API Key 回收站 error.code = %q，期望 UNAUTHORIZED", code)
		}
	})

	t.Run("deleted-requires-mine", func(t *testing.T) {
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images?deleted=true", "", imgTestAuth{})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("公开回收站 status = %d，期望 400，body=%s", recorder.Code, recorder.Body.String())
		}
		if code := imgTestErrorCode(t, recorder); code != "INVALID_REQUEST" {
			t.Fatalf("error.code = %q，期望 INVALID_REQUEST", code)
		}
	})

	t.Run("public-list-never-shows-deleted", func(t *testing.T) {
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images", "", imgTestAuth{})
		ids := imgTrashTestListIDs(t, recorder)
		if len(ids) != 1 || ids[0] != activeID {
			t.Fatalf("公开列表包含了已删除图片: %v", ids)
		}
		publicTrash := imgTrashTestDo(t, env, http.MethodGet, "/api/images?visibility=hidden&deleted=true", "", imgTestAuth{})
		if publicTrash.Code != http.StatusBadRequest {
			t.Fatalf("公开列表 + deleted=true status = %d，期望 400", publicTrash.Code)
		}
	})

	t.Run("trashed-alias-still-works", func(t *testing.T) {
		// First-phase spelling of the same filter; existing preview clients keep working.
		recorder := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&trashed=true", "", ownerAuth)
		if total := imgTrashTestTotal(t, recorder); total != 2 {
			t.Fatalf("trashed=true total = %d，期望 2", total)
		}
	})
}

// ---- restore ----

func TestImagesRestoreContract(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	ownerCookie := env.registerAdmin(t, "restore@example.com", "password123")
	ownerAuth := imgTestCookieAuth(ownerCookie)
	_, otherCookie := env.addUser(t, "restore-other@example.com")
	otherAuth := imgTestCookieAuth(otherCookie)
	keyAuth := imgTestBearerAuth(env.addAPIKey(t, 1))

	imageID, data := imgTrashTestUpload(t, env, ownerAuth, "restore.png", []string{"red", "blue"})
	imgTestSetHidden(t, env, ownerAuth, imageID, false)
	imgTestTrash(t, env, ownerAuth, imageID)
	originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
	_, trashedDeletedAt, _, trashedUpdatedAt := imgTrashTestRow(t, env, imageID)
	if trashedDeletedAt == nil {
		t.Fatalf("前置状态：图片应处于回收站")
	}
	trashedUsed := imgTrashTestUsedBytes(t, env, 1)
	if trashedUsed <= 0 {
		t.Fatalf("回收站图片应继续占用配额: %d", trashedUsed)
	}

	t.Run("rejections", func(t *testing.T) {
		if recorder := imgTrashTestDo(t, env, http.MethodPost, "/api/images/"+imageID+"/restore", "", imgTestAuth{}); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("匿名恢复 status = %d，期望 401", recorder.Code)
		}
		if recorder := imgTrashTestRestore(t, env, keyAuth, imageID); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("API Key 恢复 status = %d，期望 401", recorder.Code)
		}
		if recorder := imgTrashTestRestore(t, env, otherAuth, imageID); recorder.Code != http.StatusNotFound {
			t.Fatalf("他人恢复 status = %d，期望 404", recorder.Code)
		}
		if recorder := imgTrashTestRestore(t, env, ownerAuth, "does-not-exist"); recorder.Code != http.StatusNotFound {
			t.Fatalf("恢复不存在图片 status = %d，期望 404", recorder.Code)
		}
		// Nothing above may have changed the row or the bytes.
		exists, deletedAt, _, updatedAt := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt == nil || updatedAt != trashedUpdatedAt {
			t.Fatalf("被拒绝的恢复产生了副作用: exists=%v deletedAt=%v updatedAt=%q", exists, deletedAt, updatedAt)
		}
		if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
			t.Fatalf("被拒绝的恢复改动了原图字节")
		}
	})

	t.Run("live-image-is-409", func(t *testing.T) {
		liveID, liveData := imgTrashTestUpload(t, env, ownerAuth, "live.png", []string{"live"})
		liveOriginal, _ := imgTrashTestPaths(t, env, liveID)
		_, liveDeletedAt, _, liveUpdatedAt := imgTrashTestRow(t, env, liveID)
		usedBefore := imgTrashTestUsedBytes(t, env, 1)
		recorder := imgTrashTestRestore(t, env, ownerAuth, liveID)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("恢复未删除图片 status = %d，期望 409，body=%s", recorder.Code, recorder.Body.String())
		}
		if code := imgTestErrorCode(t, recorder); code != "NOT_IN_TRASH" {
			t.Fatalf("error.code = %q，期望 NOT_IN_TRASH", code)
		}
		_, afterDeletedAt, _, afterUpdatedAt := imgTrashTestRow(t, env, liveID)
		if afterDeletedAt != nil || liveDeletedAt != nil || afterUpdatedAt != liveUpdatedAt {
			t.Fatalf("409 恢复了不该恢复的图片")
		}
		if !bytes.Equal(imgTrashTestReadFile(t, liveOriginal), liveData) {
			t.Fatalf("409 改动了原图字节")
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usedBefore {
			t.Fatalf("409 改动了配额占用")
		}
	})

	t.Run("admin-removed-is-404", func(t *testing.T) {
		adminID, _ := imgTrashTestUpload(t, env, ownerAuth, "admin.png", nil)
		imgTestTrash(t, env, ownerAuth, adminID)
		if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedByAdmin = 1 WHERE id = ?`, adminID); err != nil {
			t.Fatalf("标记管理员下架失败: %v", err)
		}
		recorder := imgTrashTestRestore(t, env, ownerAuth, adminID)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("恢复管理员下架图片 status = %d，期望 404", recorder.Code)
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, adminID)
		if !exists || deletedAt == nil {
			t.Fatalf("被拒绝的管理员下架恢复修改了记录")
		}
	})

	t.Run("restores-tags-visibility-and-bytes", func(t *testing.T) {
		usedBefore := imgTrashTestUsedBytes(t, env, 1)
		recorder := imgTrashTestRestore(t, env, ownerAuth, imageID)
		if recorder.Code != http.StatusOK {
			t.Fatalf("恢复 status = %d，body=%s", recorder.Code, recorder.Body.String())
		}
		if body := imgTestJSON(t, recorder); body["ok"] != true {
			t.Fatalf("恢复响应必须是 {ok:true}: %s", recorder.Body.String())
		}
		exists, deletedAt, isVisible, updatedAt := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt != nil {
			t.Fatalf("恢复后 deletedAt 应为 NULL: exists=%v deletedAt=%v", exists, deletedAt)
		}
		if isVisible != 0 {
			t.Fatalf("恢复改变了可见性: isVisible=%d，期望 0", isVisible)
		}
		if updatedAt <= trashedUpdatedAt {
			t.Fatalf("恢复未更新 updatedAt: %q -> %q", trashedUpdatedAt, updatedAt)
		}
		tags := imgTrashTestTags(t, env, imageID)
		if len(tags) != 2 || tags[0] != "blue" || tags[1] != "red" {
			t.Fatalf("恢复后的标签错误: %v", tags)
		}
		if originalRel, thumbRel := imgTrashTestRelativePaths(t, env, imageID); originalRel == "" || thumbRel == "" {
			t.Fatalf("恢复清空了存储路径")
		}
		if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
			t.Fatalf("恢复后原图字节改变")
		}
		if info, err := os.Stat(thumbAbs); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("恢复后缩略图缺失: %v", err)
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usedBefore {
			t.Fatalf("恢复改变了配额占用: %d -> %d", usedBefore, used)
		}

		active := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true", "", ownerAuth)
		if ids := imgTrashTestListIDs(t, active); !imgTrashTestContains(ids, imageID) {
			t.Fatalf("恢复后个人图库缺少图片: %v", ids)
		}
		trash := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&query=restore.png", "", ownerAuth)
		if ids := imgTestListIDs(t, trash); len(ids) != 0 {
			t.Fatalf("恢复后仍出现在回收站: %v", ids)
		}
		read := imgTrashTestDo(t, env, http.MethodGet, "/api/images/"+imageID+"?variant=original", "", ownerAuth)
		// Visibility is preserved, so the hidden image still serves the fixed placeholder
		// to everyone, including the owner; the bytes themselves are asserted above.
		if read.Code != http.StatusOK || !strings.HasPrefix(read.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatalf("恢复后读取隐藏图片应返回占位图: status=%d type=%q", read.Code, read.Header().Get("Content-Type"))
		}
	})

	t.Run("second-restore-is-409", func(t *testing.T) {
		_, _, _, before := imgTrashTestRow(t, env, imageID)
		recorder := imgTrashTestRestore(t, env, ownerAuth, imageID)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("重复恢复 status = %d，期望 409", recorder.Code)
		}
		_, deletedAt, _, after := imgTrashTestRow(t, env, imageID)
		if deletedAt != nil || after != before {
			t.Fatalf("重复恢复产生了副作用: deletedAt=%v updatedAt %q -> %q", deletedAt, before, after)
		}
	})
}

// ---- permanent delete ----

func TestImagesPermanentDeleteContract(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	ownerCookie := env.registerAdmin(t, "purge@example.com", "password123")
	ownerAuth := imgTestCookieAuth(ownerCookie)
	_, otherCookie := env.addUser(t, "purge-other@example.com")
	otherAuth := imgTestCookieAuth(otherCookie)
	keyAuth := imgTestBearerAuth(env.addAPIKey(t, 1))

	imageID, data := imgTrashTestUpload(t, env, ownerAuth, "purge-a.png", []string{"one", "two"})
	originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
	keepID, keepData := imgTrashTestUpload(t, env, ownerAuth, "purge-b.png", nil)
	keepOriginal, keepThumb := imgTrashTestPaths(t, env, keepID)
	imgTestTrash(t, env, ownerAuth, imageID)

	t.Run("live-image-is-409-without-side-effects", func(t *testing.T) {
		usedBefore := imgTrashTestUsedBytes(t, env, 1)
		recorder := imgTrashTestPurge(t, env, ownerAuth, keepID)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("彻底删除未删除图片 status = %d，期望 409，body=%s", recorder.Code, recorder.Body.String())
		}
		if code := imgTestErrorCode(t, recorder); code != "NOT_IN_TRASH" {
			t.Fatalf("error.code = %q，期望 NOT_IN_TRASH", code)
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, keepID)
		if !exists || deletedAt != nil {
			t.Fatalf("409 修改了未删除图片的记录")
		}
		if imgTrashTestFileMissing(t, keepOriginal) || imgTrashTestFileMissing(t, keepThumb) {
			t.Fatalf("409 删除了未删除图片的文件")
		}
		if !bytes.Equal(imgTrashTestReadFile(t, keepOriginal), keepData) {
			t.Fatalf("409 改动了未删除图片的字节")
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usedBefore {
			t.Fatalf("409 释放了配额: %d -> %d", usedBefore, used)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("409 残留暂存目录: %v", leftovers)
		}
	})

	t.Run("other-user-unknown-and-key", func(t *testing.T) {
		usedBefore := imgTrashTestUsedBytes(t, env, 1)
		if recorder := imgTrashTestPurge(t, env, otherAuth, imageID); recorder.Code != http.StatusNotFound {
			t.Fatalf("他人彻底删除 status = %d，期望 404", recorder.Code)
		}
		if recorder := imgTrashTestPurge(t, env, ownerAuth, "does-not-exist"); recorder.Code != http.StatusNotFound {
			t.Fatalf("彻底删除不存在图片 status = %d，期望 404", recorder.Code)
		}
		if recorder := imgTrashTestPurge(t, env, imgTestAuth{}, imageID); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("匿名彻底删除 status = %d，期望 401", recorder.Code)
		}
		if recorder := imgTrashTestPurge(t, env, keyAuth, imageID); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("API Key 彻底删除 status = %d，期望 401", recorder.Code)
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt == nil {
			t.Fatalf("被拒绝的彻底删除修改了记录")
		}
		if imgTrashTestFileMissing(t, originalAbs) || imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("被拒绝的彻底删除删除了文件")
		}
		if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
			t.Fatalf("被拒绝的彻底删除改动了原图字节")
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usedBefore {
			t.Fatalf("被拒绝的彻底删除释放了配额")
		}
	})

	t.Run("admin-removed-is-404", func(t *testing.T) {
		adminID, _ := imgTrashTestUpload(t, env, ownerAuth, "admin-purge.png", nil)
		imgTestTrash(t, env, ownerAuth, adminID)
		if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedByAdmin = 1 WHERE id = ?`, adminID); err != nil {
			t.Fatalf("标记管理员下架失败: %v", err)
		}
		adminOriginal, adminThumb := imgTrashTestPaths(t, env, adminID)
		recorder := imgTrashTestPurge(t, env, ownerAuth, adminID)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("彻底删除管理员下架图片 status = %d，期望 404", recorder.Code)
		}
		if exists, _, _, _ := imgTrashTestRow(t, env, adminID); !exists {
			t.Fatalf("管理员下架图片被上传者彻底删除了")
		}
		if imgTrashTestFileMissing(t, adminOriginal) || imgTrashTestFileMissing(t, adminThumb) {
			t.Fatalf("管理员下架图片的文件被上传者彻底删除")
		}
	})

	t.Run("purge-removes-row-tags-and-files", func(t *testing.T) {
		usageBefore := imgTrashTestUsedBytes(t, env, 1)
		entriesBefore := imgTestImagesDirEntries(t, env.app.cfg.DataDir)
		recorder := imgTrashTestPurge(t, env, ownerAuth, imageID)
		if recorder.Code != http.StatusOK {
			t.Fatalf("彻底删除 status = %d，body=%s", recorder.Code, recorder.Body.String())
		}
		if body := imgTestJSON(t, recorder); body["ok"] != true {
			t.Fatalf("彻底删除响应必须是 {ok:true}: %s", recorder.Body.String())
		}
		if pending := imgTestJSON(t, recorder)["cleanupPending"]; pending != false {
			t.Fatalf("成功清理后 cleanupPending 应为 false: %s", recorder.Body.String())
		}
		if exists, _, _, _ := imgTrashTestRow(t, env, imageID); exists {
			t.Fatalf("彻底删除后数据库仍有记录")
		}
		var tagRows int
		if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "ImageTag" WHERE imageId = ?`, imageID).Scan(&tagRows); err != nil {
			t.Fatalf("统计标签失败: %v", err)
		}
		if tagRows != 0 {
			t.Fatalf("彻底删除后仍有 %d 个标签", tagRows)
		}
		if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("彻底删除后原图或缩略图仍存在")
		}
		entriesAfter := imgTestImagesDirEntries(t, env.app.cfg.DataDir)
		if len(entriesAfter) != len(entriesBefore)-2 {
			t.Fatalf("彻底删除后文件数 = %d，期望 %d: %v", len(entriesAfter), len(entriesBefore)-2, entriesAfter)
		}
		if got := imgTrashTestDo(t, env, http.MethodGet, "/api/images/"+imageID+"?variant=original", "", ownerAuth); got.Code != http.StatusNotFound {
			t.Fatalf("彻底删除后读取原图 status = %d，期望 404", got.Code)
		}
		if trash := imgTrashTestDo(t, env, http.MethodGet, "/api/images?mine=true&deleted=true&query=purge-a", "", ownerAuth); imgTrashTestTotal(t, trash) != 0 {
			t.Fatalf("彻底删除后回收站仍有记录: %s", trash.Body.String())
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usageBefore-int64(len(data)) {
			t.Fatalf("彻底删除后已用配额 = %d，期望 %d（配额必须按 size 释放）", used, usageBefore-int64(len(data)))
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("彻底删除后残留暂存字节: %v", leftovers)
		}
		if !bytes.Equal(imgTrashTestReadFile(t, keepOriginal), keepData) {
			t.Fatalf("彻底删除影响了其他图片的字节")
		}
	})

	t.Run("second-purge-is-404", func(t *testing.T) {
		if recorder := imgTrashTestPurge(t, env, ownerAuth, imageID); recorder.Code != http.StatusNotFound {
			t.Fatalf("重复彻底删除 status = %d，期望 404", recorder.Code)
		}
	})
}

func TestImagesPermanentDeleteReleasesQuota(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "purge-quota@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	fixture := imgTestPNG(t, 44, 33)
	size := int64(len(fixture))
	env.setQuota(t, 1, size*2)

	first, firstBody := env.imgTestUploadBytes(t, auth, "quota-a.png", fixture, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", first.Code, first.Body.String())
	}
	imageID := imgTestMustImageID(t, firstBody)
	second, secondBody := env.imgTestUploadBytes(t, auth, "quota-b.png", fixture, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", second.Code, second.Body.String())
	}
	imgTestMustImageID(t, secondBody)

	over := env.upload(t, auth, imgTestUploadOptions{FileName: "over.png", Data: fixture})
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("配额已满时上传 status = %d，期望 413，body=%s", over.Code, over.Body.String())
	}

	imgTestTrash(t, env, auth, imageID)
	if used := imgTrashTestUsedBytes(t, env, 1); used != size*2 {
		t.Fatalf("回收站图片应继续占用配额: used=%d，期望 %d", used, size*2)
	}
	stillBlocked := env.upload(t, auth, imgTestUploadOptions{FileName: "blocked.png", Data: fixture})
	if stillBlocked.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("回收站占用配额时上传 status = %d，期望 413", stillBlocked.Code)
	}

	if recorder := imgTrashTestPurge(t, env, auth, imageID); recorder.Code != http.StatusOK {
		t.Fatalf("彻底删除 status = %d，body=%s", recorder.Code, recorder.Body.String())
	}
	if used := imgTrashTestUsedBytes(t, env, 1); used != size {
		t.Fatalf("彻底删除后已用配额 = %d，期望 %d", used, size)
	}
	afterPurge := env.upload(t, auth, imgTestUploadOptions{FileName: "after-purge.png", Data: fixture})
	if afterPurge.Code != http.StatusOK {
		t.Fatalf("配额释放后上传失败: status=%d body=%s", afterPurge.Code, afterPurge.Body.String())
	}
	if used := imgTrashTestUsedBytes(t, env, 1); used != size*2 {
		t.Fatalf("配额释放后已用 = %d，期望 %d", used, size*2)
	}
}

func TestImagesPermanentDeleteFailures(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "purge-fail@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	t.Run("file-failure-rolls-back", func(t *testing.T) {
		imageID, data := imgTrashTestUpload(t, env, auth, "file-fail.png", []string{"keep"})
		imgTestTrash(t, env, auth, imageID)
		originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
		originalRel, thumbRel := imgTrashTestRelativePaths(t, env, imageID)
		entriesBefore := imgTestImagesDirEntries(t, env.app.cfg.DataDir)
		usageBefore := imgTrashTestUsedBytes(t, env, 1)
		_, _, _, updatedBefore := imgTrashTestRow(t, env, imageID)

		// A regular file where a directory is required makes os.Rename fail with a real
		// filesystem error (ENOTDIR) instead of a synthetic test hook. The original is
		// moved first and must be moved back when the thumbnail cannot be quarantined.
		blocker := filepath.Join(env.app.cfg.DataDir, "images", "blocker")
		if err := os.WriteFile(blocker, []byte("blocker"), 0o600); err != nil {
			t.Fatalf("创建阻塞文件失败: %v", err)
		}
		if _, err := env.app.db.Exec(`UPDATE "Image" SET thumbPath = 'images/blocker/thumb.webp' WHERE id = ?`, imageID); err != nil {
			t.Fatalf("构造失败路径失败: %v", err)
		}

		recorder := imgTrashTestPurge(t, env, auth, imageID)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("文件失败时 status = %d，期望 500，body=%s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"ok":true`) {
			t.Fatalf("文件失败时不得声称成功: %s", recorder.Body.String())
		}
		exists, deletedAt, _, updatedAfter := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt == nil {
			t.Fatalf("文件失败后记录被删除")
		}
		if updatedAfter != updatedBefore {
			t.Fatalf("文件失败后 updatedAt 被改动: %q -> %q", updatedBefore, updatedAfter)
		}
		if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
			t.Fatalf("文件失败后原图字节丢失或改变")
		}
		if tags := imgTrashTestTags(t, env, imageID); len(tags) != 1 || tags[0] != "keep" {
			t.Fatalf("文件失败后标签被改动: %v", tags)
		}
		if entries := imgTestImagesDirEntries(t, env.app.cfg.DataDir); len(entries) != len(entriesBefore)+1 {
			t.Fatalf("文件失败后文件集合异常: %v", entries)
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usageBefore {
			t.Fatalf("文件失败后配额被释放: %d -> %d", usageBefore, used)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("文件失败后残留暂存目录: %v", leftovers)
		}

		// Repair both the row and the blocker: the retried purge must converge.
		if _, err := env.app.db.Exec(`UPDATE "Image" SET thumbPath = ? WHERE id = ?`, thumbRel, imageID); err != nil {
			t.Fatalf("恢复缩略图路径失败: %v", err)
		}
		if err := os.Remove(blocker); err != nil {
			t.Fatalf("清理阻塞文件失败: %v", err)
		}
		if retry := imgTrashTestPurge(t, env, auth, imageID); retry.Code != http.StatusOK {
			t.Fatalf("修复后重试彻底删除 status = %d，body=%s", retry.Code, retry.Body.String())
		}
		if exists, _, _, _ := imgTrashTestRow(t, env, imageID); exists {
			t.Fatalf("修复后重试未删除记录")
		}
		if !imgTrashTestFileMissing(t, originalAbs) {
			t.Fatalf("修复后重试未删除原图（原图路径 %s）", originalRel)
		}
		if !imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("修复后重试未删除缩略图")
		}
	})

	t.Run("database-failure-restores-files", func(t *testing.T) {
		imageID, data := imgTrashTestUpload(t, env, auth, "db-fail.png", []string{"alpha", "beta"})
		imgTestTrash(t, env, auth, imageID)
		originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
		usageBefore := imgTrashTestUsedBytes(t, env, 1)
		thumbBytes := imgTrashTestReadFile(t, thumbAbs)

		if _, err := env.app.db.Exec(`CREATE TRIGGER trash_test_block_purge BEFORE DELETE ON "Image"
			BEGIN SELECT RAISE(ABORT, 'forced purge failure'); END`); err != nil {
			t.Fatalf("创建失败触发器失败: %v", err)
		}
		recorder := imgTrashTestPurge(t, env, auth, imageID)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("数据库失败时 status = %d，期望 500，body=%s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"ok":true`) {
			t.Fatalf("数据库失败时不得声称成功: %s", recorder.Body.String())
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt == nil {
			t.Fatalf("数据库失败后记录状态异常: exists=%v deletedAt=%v", exists, deletedAt)
		}
		if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
			t.Fatalf("数据库失败后原图未搬回")
		}
		if !bytes.Equal(imgTrashTestReadFile(t, thumbAbs), thumbBytes) {
			t.Fatalf("数据库失败后缩略图未搬回")
		}
		if tags := imgTrashTestTags(t, env, imageID); len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
			t.Fatalf("数据库失败后标签未回滚: %v", tags)
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usageBefore {
			t.Fatalf("数据库失败后配额被误释放: %d -> %d", usageBefore, used)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("数据库失败后残留暂存目录: %v", leftovers)
		}

		if _, err := env.app.db.Exec(`DROP TRIGGER trash_test_block_purge`); err != nil {
			t.Fatalf("删除失败触发器失败: %v", err)
		}
		if retry := imgTrashTestPurge(t, env, auth, imageID); retry.Code != http.StatusOK {
			t.Fatalf("移除故障后重试 status = %d，body=%s", retry.Code, retry.Body.String())
		}
		if exists, _, _, _ := imgTrashTestRow(t, env, imageID); exists {
			t.Fatalf("重试后记录仍存在")
		}
		if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("重试后文件仍存在")
		}
	})
}

// ---- concurrency ----

func TestImagesTrashConcurrency(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "trash-race@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	type outcome struct {
		op     string
		status int
	}
	run := func(t *testing.T, imageID string, ops []string) []outcome {
		t.Helper()
		results := make([]outcome, len(ops))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for index, op := range ops {
			wait.Add(1)
			go func(index int, op string) {
				defer wait.Done()
				<-start
				var method, target string
				switch op {
				case "restore":
					method, target = http.MethodPost, "/api/images/"+imageID+"/restore"
				case "purge":
					method, target = http.MethodDelete, "/api/images/"+imageID+"?permanent=true"
				}
				request := httptest.NewRequest(method, target, nil)
				request.AddCookie(cookie)
				recorder := httptest.NewRecorder()
				env.handler.ServeHTTP(recorder, request)
				results[index] = outcome{op: op, status: recorder.Code}
			}(index, op)
		}
		close(start)
		wait.Wait()
		return results
	}
	countStatus := func(results []outcome, status int) int {
		count := 0
		for _, result := range results {
			if result.status == status {
				count++
			}
		}
		return count
	}
	countOp := func(results []outcome, op string, status int) int {
		count := 0
		for _, result := range results {
			if result.op == op && result.status == status {
				count++
			}
		}
		return count
	}

	t.Run("purge-versus-restore", func(t *testing.T) {
		imageID, data := imgTrashTestUpload(t, env, auth, "race-mixed.png", []string{"race"})
		imgTestTrash(t, env, auth, imageID)
		originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)

		results := run(t, imageID, []string{"purge", "restore", "purge", "restore", "purge", "restore"})
		if ok := countStatus(results, http.StatusOK); ok != 1 {
			t.Fatalf("并发混合操作成功次数 = %d，期望 1: %+v", ok, results)
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID)
		switch {
		case exists && deletedAt == nil:
			// Restore won: the image is fully intact and every purge saw a live row.
			if conflicted := countOp(results, "purge", http.StatusConflict); conflicted != 3 {
				t.Fatalf("恢复胜出时彻底删除应全部 409，实际 %d: %+v", conflicted, results)
			}
			if restored := countOp(results, "restore", http.StatusOK); restored != 1 {
				t.Fatalf("恢复胜出时只应有一个恢复成功，实际 %d: %+v", restored, results)
			}
			if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
				t.Fatalf("恢复胜出后原图字节改变")
			}
			if imgTrashTestFileMissing(t, thumbAbs) {
				t.Fatalf("恢复胜出后缩略图丢失")
			}
			if tags := imgTrashTestTags(t, env, imageID); len(tags) != 1 || tags[0] != "race" {
				t.Fatalf("恢复胜出后标签被改动: %v", tags)
			}
		case !exists:
			// Purge won: every other call observed a missing row.
			if purged := countOp(results, "purge", http.StatusOK); purged != 1 {
				t.Fatalf("删除胜出时只应有一个删除成功，实际 %d: %+v", purged, results)
			}
			if missing := countOp(results, "restore", http.StatusNotFound); missing != 3 {
				t.Fatalf("删除胜出时恢复应全部 404，实际 %d: %+v", missing, results)
			}
			if missing := countOp(results, "purge", http.StatusNotFound); missing != 2 {
				t.Fatalf("删除胜出时其余彻底删除应为 404，实际 %d: %+v", missing, results)
			}
			if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
				t.Fatalf("删除胜出后文件仍存在")
			}
		default:
			t.Fatalf("并发后记录状态非法: exists=%v deletedAt=%v", exists, deletedAt)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("并发后残留暂存目录: %v", leftovers)
		}
	})

	t.Run("concurrent-purges", func(t *testing.T) {
		imageID, data := imgTrashTestUpload(t, env, auth, "race-purge.png", nil)
		imgTestTrash(t, env, auth, imageID)
		originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
		usageBefore := imgTrashTestUsedBytes(t, env, 1)

		results := run(t, imageID, []string{"purge", "purge", "purge", "purge"})
		if ok := countStatus(results, http.StatusOK); ok != 1 {
			t.Fatalf("并发彻底删除成功次数 = %d，期望 1: %+v", ok, results)
		}
		if missing := countStatus(results, http.StatusNotFound); missing != 3 {
			t.Fatalf("并发彻底删除 404 次数 = %d，期望 3: %+v", missing, results)
		}
		if exists, _, _, _ := imgTrashTestRow(t, env, imageID); exists {
			t.Fatalf("并发彻底删除后记录仍存在")
		}
		if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("并发彻底删除后文件仍存在")
		}
		if used := imgTrashTestUsedBytes(t, env, 1); used != usageBefore-int64(len(data)) {
			t.Fatalf("并发彻底删除后配额 = %d，期望 %d", used, usageBefore-int64(len(data)))
		}
		if tags := imgTrashTestTags(t, env, imageID); len(tags) != 0 {
			t.Fatalf("并发彻底删除后仍残留标签: %v", tags)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("并发彻底删除后残留暂存目录: %v", leftovers)
		}
	})

	t.Run("concurrent-restores", func(t *testing.T) {
		imageID, _ := imgTrashTestUpload(t, env, auth, "race-restore.png", nil)
		imgTestTrash(t, env, auth, imageID)
		results := run(t, imageID, []string{"restore", "restore", "restore"})
		if ok := countStatus(results, http.StatusOK); ok != 1 {
			t.Fatalf("并发恢复成功次数 = %d，期望 1: %+v", ok, results)
		}
		if conflicts := countStatus(results, http.StatusConflict); conflicts != 2 {
			t.Fatalf("并发恢复 409 次数 = %d，期望 2: %+v", conflicts, results)
		}
		exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID)
		if !exists || deletedAt != nil {
			t.Fatalf("并发恢复后状态异常: exists=%v deletedAt=%v", exists, deletedAt)
		}
	})
}

// ---- cross-origin writes ----

func TestImagesTrashCrossOriginWrites(t *testing.T) {
	env := imgTestNewApp(t, Config{BaseURL: "https://pic.example.com"})
	cookie := env.registerAdmin(t, "trash-origin@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, _ := imgTrashTestUpload(t, env, auth, "origin.png", nil)
	imgTestTrash(t, env, auth, imageID)

	deny := imgTrashTestDo(t, env, http.MethodPost, "/api/images/"+imageID+"/restore", "https://evil.example", auth)
	if deny.Code != http.StatusForbidden {
		t.Fatalf("跨源恢复 status = %d，期望 403，body=%s", deny.Code, deny.Body.String())
	}
	if code := imgTestErrorCode(t, deny); code != "CROSS_ORIGIN_DENIED" {
		t.Fatalf("跨源恢复 error.code = %q", code)
	}
	if exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID); !exists || deletedAt == nil {
		t.Fatalf("跨源恢复修改了记录")
	}
	allow := imgTrashTestDo(t, env, http.MethodPost, "/api/images/"+imageID+"/restore", "https://pic.example.com", auth)
	if allow.Code != http.StatusOK {
		t.Fatalf("同源恢复 status = %d，body=%s", allow.Code, allow.Body.String())
	}

	imgTestTrash(t, env, auth, imageID)
	denyPurge := imgTrashTestDo(t, env, http.MethodDelete, "/api/images/"+imageID+"?permanent=true", "https://evil.example", auth)
	if denyPurge.Code != http.StatusForbidden {
		t.Fatalf("跨源彻底删除 status = %d，期望 403，body=%s", denyPurge.Code, denyPurge.Body.String())
	}
	if exists, _, _, _ := imgTrashTestRow(t, env, imageID); !exists {
		t.Fatalf("跨源彻底删除删除了记录")
	}
	allowPurge := imgTrashTestDo(t, env, http.MethodDelete, "/api/images/"+imageID+"?permanent=true", "https://pic.example.com", auth)
	if allowPurge.Code != http.StatusOK {
		t.Fatalf("同源彻底删除 status = %d，body=%s", allowPurge.Code, allowPurge.Body.String())
	}
}

// ---- crash windows around the purge quarantine ----

// TestImagesPurgeRecoversStagedCrash covers the first crash window: the files were
// renamed into the quarantine and the process died before the transaction committed.
// The database row is therefore still there, and a restart must put the bytes back.
func TestImagesPurgeRecoversStagedCrash(t *testing.T) {
	dataDir := t.TempDir()
	first := imgTestNewApp(t, Config{DataDir: dataDir})
	cookie := first.registerAdmin(t, "crash-staged@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, data := imgTrashTestUpload(t, first, auth, "crash-staged.png", []string{"alpha", "beta"})
	imgTestTrash(t, first, auth, imageID)
	originalAbs, thumbAbs := imgTrashTestPaths(t, first, imageID)
	originalRel, thumbRel := imgTrashTestRelativePaths(t, first, imageID)
	thumbBytes := imgTrashTestReadFile(t, thumbAbs)
	usage := imgTrashTestUsedBytes(t, first, 1)

	// Exactly the state the purge leaves behind between "files renamed" and "commit".
	quarantine, err := first.app.imagesStagePurge(imageID, []string{originalRel, thumbRel})
	if err != nil {
		t.Fatalf("构造崩溃现场失败: %v", err)
	}
	if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
		t.Fatalf("崩溃现场的前置条件错误：原图/缩略图仍在规范路径")
	}
	rawManifest, err := os.ReadFile(filepath.Join(quarantine.dir, imagesPurgeManifestName))
	if err != nil {
		t.Fatalf("暂存目录缺少清单: %v", err)
	}
	var manifest imagesPurgeManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if manifest.ImageID != imageID {
		t.Fatalf("清单 imageId = %q，期望 %q", manifest.ImageID, imageID)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Relative != originalRel || manifest.Files[1].Relative != thumbRel {
		t.Fatalf("清单存储路径错误: %+v", manifest.Files)
	}
	if err := first.app.Close(); err != nil {
		t.Fatalf("模拟进程退出失败: %v", err)
	}

	restarted := imgTestNewApp(t, Config{DataDir: dataDir})
	if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
		t.Fatalf("重启恢复后原图字节不一致")
	}
	if !bytes.Equal(imgTrashTestReadFile(t, thumbAbs), thumbBytes) {
		t.Fatalf("重启恢复后缩略图字节不一致")
	}
	exists, deletedAt, _, _ := imgTrashTestRow(t, restarted, imageID)
	if !exists || deletedAt == nil {
		t.Fatalf("重启恢复不应改动数据库状态: exists=%v deletedAt=%v", exists, deletedAt)
	}
	if tags := imgTrashTestTags(t, restarted, imageID); len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
		t.Fatalf("重启恢复改动了标签: %v", tags)
	}
	if used := imgTrashTestUsedBytes(t, restarted, 1); used != usage {
		t.Fatalf("重启恢复改动了配额占用: %d -> %d", usage, used)
	}
	if leftovers := imgTrashTestLeftovers(t, dataDir); len(leftovers) != 0 {
		t.Fatalf("重启恢复后残留暂存目录: %v", leftovers)
	}
	// The bytes are back, so this restore is no longer a false success.
	if recorder := imgTrashTestRestore(t, restarted, auth, imageID); recorder.Code != http.StatusOK {
		t.Fatalf("重启恢复后恢复图片 status = %d，body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestImagesPurgeRecoversCommittedCrash covers the second crash window: the row (and
// its tags) were committed as deleted, but the process died before the quarantined
// bytes were removed. A restart must drop the residue and never resurrect the row.
func TestImagesPurgeRecoversCommittedCrash(t *testing.T) {
	dataDir := t.TempDir()
	first := imgTestNewApp(t, Config{DataDir: dataDir})
	cookie := first.registerAdmin(t, "crash-committed@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, data := imgTrashTestUpload(t, first, auth, "crash-committed.png", nil)
	keepID, _ := imgTrashTestUpload(t, first, auth, "crash-committed-keep.png", nil)
	imgTestTrash(t, first, auth, imageID)
	originalAbs, thumbAbs := imgTrashTestPaths(t, first, imageID)
	originalRel, thumbRel := imgTrashTestRelativePaths(t, first, imageID)
	usageBefore := imgTrashTestUsedBytes(t, first, 1)

	if _, err := first.app.imagesStagePurge(imageID, []string{originalRel, thumbRel}); err != nil {
		t.Fatalf("构造崩溃现场失败: %v", err)
	}
	// The committed transaction the purge would have run.
	if _, err := first.app.db.Exec(`DELETE FROM "ImageTag" WHERE imageId = ?`, imageID); err != nil {
		t.Fatalf("模拟提交删除标签失败: %v", err)
	}
	if _, err := first.app.db.Exec(`DELETE FROM "Image" WHERE id = ?`, imageID); err != nil {
		t.Fatalf("模拟提交删除记录失败: %v", err)
	}
	if err := first.app.Close(); err != nil {
		t.Fatalf("模拟进程退出失败: %v", err)
	}

	restarted := imgTestNewApp(t, Config{DataDir: dataDir})
	if exists, _, _, _ := imgTrashTestRow(t, restarted, imageID); exists {
		t.Fatalf("重启恢复复活了已提交删除的记录")
	}
	if tags := imgTrashTestTags(t, restarted, imageID); len(tags) != 0 {
		t.Fatalf("重启恢复复活了已删除的标签: %v", tags)
	}
	// Residue is cleaned instead of being moved back to the canonical paths.
	if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
		t.Fatalf("已提交删除的图片被搬回了规范路径")
	}
	if leftovers := imgTrashTestLeftovers(t, dataDir); len(leftovers) != 0 {
		t.Fatalf("重启恢复后残留暂存字节: %v", leftovers)
	}
	if used := imgTrashTestUsedBytes(t, restarted, 1); used != usageBefore-int64(len(data)) {
		t.Fatalf("重启恢复后配额 = %d，期望 %d", used, usageBefore-int64(len(data)))
	}
	if exists, deletedAt, _, _ := imgTrashTestRow(t, restarted, keepID); !exists || deletedAt != nil {
		t.Fatalf("重启恢复影响了其他图片: exists=%v deletedAt=%v", exists, deletedAt)
	}
}

// TestImagesPurgeRecoveryRejectsBadManifests proves the startup recovery fails loudly
// and preserves evidence instead of following a tampered manifest or deleting bytes it
// cannot explain.
func TestImagesPurgeRecoveryRejectsBadManifests(t *testing.T) {
	writeQuarantine := func(t *testing.T, dataDir, name, content string) string {
		t.Helper()
		dir := filepath.Join(dataDir, imagesTempDirName, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("创建残留目录失败: %v", err)
		}
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, imagesPurgeManifestName), []byte(content), 0o600); err != nil {
				t.Fatalf("写入清单失败: %v", err)
			}
		}
		return dir
	}
	assertRetained := func(t *testing.T, dir string) {
		t.Helper()
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("启动失败后必须保留现场 %s: %v", dir, err)
		}
	}

	cases := []struct {
		name     string
		manifest string
		// stashed 表示"没有最终清单、但已经搬过字节"：缺失清单本身不再算坏现场
		// （清单在任何 rename 之前落盘，见 TestImagesPurgeRecoveryCleansPreManifestWindow），
		// 只有同时出现无法解释的条目（暂存字节/未知文件/符号链接）才必须保留现场。
		stashed bool
	}{
		{
			name:     "escaping-relative",
			manifest: `{"imageId":"img-1","files":[{"name":"file-0","relative":"../../escape.png"}]}`,
		},
		{
			name:     "absolute-relative",
			manifest: `{"imageId":"img-1","files":[{"name":"file-0","relative":"/etc/passwd"}]}`,
		},
		{
			name:     "escaping-stash-name",
			manifest: `{"imageId":"img-1","files":[{"name":"../escape.png","relative":"images/1/x.png"}]}`,
		},
		{
			name:     "not-json",
			manifest: `{`,
		},
		{
			name:     "empty-files",
			manifest: `{"imageId":"img-1","files":[]}`,
		},
		{
			name:     "missing-manifest-with-stashed-bytes",
			manifest: "",
			stashed:  true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			dir := writeQuarantine(t, dataDir, "purge-broken", testCase.manifest)
			if testCase.stashed {
				if err := os.WriteFile(filepath.Join(dir, imagesPurgeStashName(0)), []byte("stashed"), 0o600); err != nil {
					t.Fatalf("写入暂存字节失败: %v", err)
				}
			}
			app, err := New(Config{DataDir: dataDir})
			if err == nil {
				_ = app.Close()
				t.Fatalf("非法回收站清理现场必须让启动失败")
			}
			if !strings.Contains(err.Error(), dir) {
				t.Fatalf("启动错误必须指向残留目录 %s，实际: %v", dir, err)
			}
			assertRetained(t, dir)
			// Nothing may be created outside the data directory.
			outside := filepath.Join(filepath.Dir(dataDir), "escape.png")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Fatalf("清单路径逃逸被执行了: %s", outside)
			}
		})
	}

	t.Run("unexpected-file-instead-of-directory", func(t *testing.T) {
		dataDir := t.TempDir()
		tempDir := filepath.Join(dataDir, imagesTempDirName)
		if err := os.MkdirAll(tempDir, 0o700); err != nil {
			t.Fatalf("创建暂存目录失败: %v", err)
		}
		stray := filepath.Join(tempDir, imagesPurgeQuarantinePrefix+"stray")
		if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
			t.Fatalf("创建残留文件失败: %v", err)
		}
		app, err := New(Config{DataDir: dataDir})
		if err == nil {
			_ = app.Close()
			t.Fatalf("无法识别的回收站清理残留必须让启动失败")
		}
		assertRetained(t, stray)
	})
}

// TestImagesPurgeCleanupPendingFlag covers the honest success response: when the bytes
// cannot be removed after the commit, the row is gone but the caller is told that disk
// cleanup is still pending, and the residue is logged with its path.
func TestImagesPurgeCleanupPendingFlag(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "cleanup-pending@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, _ := imgTrashTestUpload(t, env, auth, "cleanup-pending.png", nil)
	imgTestTrash(t, env, auth, imageID)
	originalRel, thumbRel := imgTrashTestRelativePaths(t, env, imageID)
	quarantine, err := env.app.imagesStagePurge(imageID, []string{originalRel, thumbRel})
	if err != nil {
		t.Fatalf("构造暂存目录失败: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("需要非 root 才能让目录删除失败")
	}
	if err := os.Chmod(quarantine.dir, 0o500); err != nil {
		t.Fatalf("收紧暂存目录权限失败: %v", err)
	}
	if pending := quarantine.discard(); !pending {
		t.Fatalf("目录删除失败时必须报告 cleanupPending=true")
	}
	if entries, err := os.ReadDir(quarantine.dir); err != nil || len(entries) == 0 {
		t.Fatalf("cleanupPending 时必须保留可排查证据: entries=%d err=%v", len(entries), err)
	}
	if err := os.Chmod(quarantine.dir, 0o700); err != nil {
		t.Fatalf("恢复暂存目录权限失败: %v", err)
	}
	if pending := quarantine.discard(); pending {
		t.Fatalf("权限恢复后 cleanupPending 应为 false")
	}
	if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
		t.Fatalf("清理完成后仍残留暂存目录: %v", leftovers)
	}
}

// TestImagesRestoreRejectsMissingOriginal covers the second hardening point: a restore
// must not report success when the original bytes are gone.
func TestImagesRestoreRejectsMissingOriginal(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "restore-missing@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, _ := imgTrashTestUpload(t, env, auth, "restore-missing.png", []string{"keep"})
	imgTestTrash(t, env, auth, imageID)
	originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
	usageBefore := imgTrashTestUsedBytes(t, env, 1)
	if err := os.Remove(originalAbs); err != nil {
		t.Fatalf("删除原图失败: %v", err)
	}

	recorder := imgTrashTestRestore(t, env, auth, imageID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("原图缺失时恢复 status = %d，期望 409，body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "IMAGE_BYTES_MISSING" {
		t.Fatalf("error.code = %q，期望 IMAGE_BYTES_MISSING", code)
	}
	exists, deletedAt, _, _ := imgTrashTestRow(t, env, imageID)
	if !exists || deletedAt == nil {
		t.Fatalf("被拒绝的恢复修改了数据库状态: exists=%v deletedAt=%v", exists, deletedAt)
	}
	if tags := imgTrashTestTags(t, env, imageID); len(tags) != 1 || tags[0] != "keep" {
		t.Fatalf("被拒绝的恢复改动了标签: %v", tags)
	}
	if used := imgTrashTestUsedBytes(t, env, 1); used != usageBefore {
		t.Fatalf("被拒绝的恢复改动了配额: %d -> %d", usageBefore, used)
	}
	if _, err := os.Stat(thumbAbs); err != nil {
		t.Fatalf("被拒绝的恢复删除了缩略图: %v", err)
	}
	if again := imgTrashTestRestore(t, env, auth, imageID); again.Code != http.StatusConflict {
		t.Fatalf("重复尝试 status = %d，期望 409", again.Code)
	}
}

// TestImagesPurgeRecoversManifestOnlyCrash covers the third window: the manifest was
// durable but no file had been renamed yet. Recovery must leave the bytes untouched,
// drop the empty quarantine and keep the row restorable.
func TestImagesPurgeRecoversManifestOnlyCrash(t *testing.T) {
	dataDir := t.TempDir()
	first := imgTestNewApp(t, Config{DataDir: dataDir})
	cookie := first.registerAdmin(t, "crash-manifest@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, data := imgTrashTestUpload(t, first, auth, "crash-manifest.png", nil)
	imgTestTrash(t, first, auth, imageID)
	originalAbs, thumbAbs := imgTrashTestPaths(t, first, imageID)
	originalRel, thumbRel := imgTrashTestRelativePaths(t, first, imageID)

	dir, err := os.MkdirTemp(filepath.Join(dataDir, imagesTempDirName), imagesPurgeQuarantinePrefix+"*")
	if err != nil {
		t.Fatalf("创建暂存目录失败: %v", err)
	}
	manifest := imagesPurgeManifest{ImageID: imageID, Files: []imagesPurgeManifestFile{
		{Name: imagesPurgeStashName(0), Relative: originalRel},
		{Name: imagesPurgeStashName(1), Relative: thumbRel},
	}}
	if err := imagesWritePurgeManifest(dir, manifest); err != nil {
		t.Fatalf("写入清单失败: %v", err)
	}
	if err := first.app.Close(); err != nil {
		t.Fatalf("模拟进程退出失败: %v", err)
	}

	restarted := imgTestNewApp(t, Config{DataDir: dataDir})
	if !bytes.Equal(imgTrashTestReadFile(t, originalAbs), data) {
		t.Fatalf("清单阶段崩溃后重启改动了原图")
	}
	if imgTrashTestFileMissing(t, thumbAbs) {
		t.Fatalf("清单阶段崩溃后重启删除了缩略图")
	}
	if exists, deletedAt, _, _ := imgTrashTestRow(t, restarted, imageID); !exists || deletedAt == nil {
		t.Fatalf("清单阶段崩溃后重启改动了数据库状态: exists=%v deletedAt=%v", exists, deletedAt)
	}
	if leftovers := imgTrashTestLeftovers(t, dataDir); len(leftovers) != 0 {
		t.Fatalf("清单阶段崩溃后残留暂存目录: %v", leftovers)
	}
	if recorder := imgTrashTestRestore(t, restarted, auth, imageID); recorder.Code != http.StatusOK {
		t.Fatalf("清单阶段崩溃后恢复图片 status = %d，body=%s", recorder.Code, recorder.Body.String())
	}
}

// ---- 评审 P2-1：manifest 发布（rename）之前的崩溃窗口 ----
//
// imagesStagePurge 先 os.MkdirTemp，再写 manifest.json.tmp，最后 rename 成 manifest.json；
// 清单在**任何** rename 之前落盘，所以"没有最终清单"本身就证明没搬过字节。旧的恢复逻辑
// 对缺失清单直接报错，导致一次极小窗口的崩溃之后服务再也起不来。

// imgTrashTestQuarantineDir 复现 imagesStagePurge 的 MkdirTemp 之后、清单发布之前的现场。
func imgTrashTestQuarantineDir(t *testing.T, dataDir string) string {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Join(dataDir, imagesTempDirName), imagesPurgeQuarantinePrefix+"*")
	if err != nil {
		t.Fatalf("构造残留目录失败: %v", err)
	}
	return dir
}

func imgTrashTestClosedApp(t *testing.T, dataDir string) *imgTestEnv {
	t.Helper()
	env := imgTestNewApp(t, Config{DataDir: dataDir})
	if err := env.app.Close(); err != nil {
		t.Fatalf("模拟进程退出失败: %v", err)
	}
	return env
}

func TestImagesPurgeRecoveryCleansPreManifestWindow(t *testing.T) {
	// 这些现场都对应"崩在 MkdirTemp 之后、rename 之前"：目录里不可能有搬移过的字节。
	cases := []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{name: "空目录", write: func(t *testing.T, dir string) {}},
		{name: "写了一半的 manifest.json.tmp", write: func(t *testing.T, dir string) {
			// 崩溃在 tmp 写入过程中：内容被截断，且从未 rename。
			partial := `{"imageId":"abc","files":[{"name":"file-0","relative":"imag`
			if err := os.WriteFile(filepath.Join(dir, imagesPurgeManifestName+imagesPurgeManifestTempSuffix), []byte(partial), 0o600); err != nil {
				t.Fatalf("写入半截清单失败: %v", err)
			}
		}},
		{name: "空的 manifest.json.tmp", write: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, imagesPurgeManifestName+imagesPurgeManifestTempSuffix), nil, 0o600); err != nil {
				t.Fatalf("写入空清单失败: %v", err)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			imgTrashTestClosedApp(t, dataDir)
			dir := imgTrashTestQuarantineDir(t, dataDir)
			testCase.write(t, dir)

			// 重启必须成功，并且把"确证未搬移字节"的残留清掉。
			restarted := imgTestNewApp(t, Config{DataDir: dataDir})
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("未搬移字节的残留目录应被清理: %v", err)
			}
			if leftovers := imgTrashTestLeftovers(t, dataDir); len(leftovers) != 0 {
				t.Fatalf("重启后仍有残留: %v", leftovers)
			}
			if err := restarted.app.Close(); err != nil {
				t.Fatalf("Close 失败: %v", err)
			}
		})
	}
}

func TestImagesPurgeRecoveryKeepsUnexplainedPreManifestState(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, dir, dataDir string)
		restore func(t *testing.T, dir, dataDir string)
		keyword string
	}{
		{name: "已搬移的暂存字节", prepare: func(t *testing.T, dir, dataDir string) {
			if err := os.WriteFile(filepath.Join(dir, imagesPurgeStashName(0)), []byte("stashed image bytes"), 0o600); err != nil {
				t.Fatalf("写入暂存字节失败: %v", err)
			}
		}, keyword: "file-0"},
		{name: "未知文件", prepare: func(t *testing.T, dir, dataDir string) {
			if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("manual note"), 0o600); err != nil {
				t.Fatalf("写入未知文件失败: %v", err)
			}
		}, keyword: "notes.txt"},
		{name: "子目录", prepare: func(t *testing.T, dir, dataDir string) {
			if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o750); err != nil {
				t.Fatalf("创建子目录失败: %v", err)
			}
		}, keyword: "nested"},
		{name: "符号链接条目", prepare: func(t *testing.T, dir, dataDir string) {
			target := filepath.Join(dataDir, "outside.bin")
			if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
				t.Fatalf("创建链接目标失败: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(dir, imagesPurgeStashName(0))); err != nil {
				t.Fatalf("创建符号链接失败: %v", err)
			}
		}, keyword: "file-0"},
		{name: "manifest.json 是符号链接", prepare: func(t *testing.T, dir, dataDir string) {
			target := filepath.Join(dataDir, "outside-manifest.json")
			if err := os.WriteFile(target, []byte(`{"imageId":"x","files":[]}`), 0o600); err != nil {
				t.Fatalf("创建链接目标失败: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(dir, imagesPurgeManifestName)); err != nil {
				t.Fatalf("创建符号链接失败: %v", err)
			}
		}, keyword: "不是常规文件"},
		{name: "manifest.json 是目录", prepare: func(t *testing.T, dir, dataDir string) {
			if err := os.MkdirAll(filepath.Join(dir, imagesPurgeManifestName), 0o750); err != nil {
				t.Fatalf("创建目录失败: %v", err)
			}
		}, keyword: "不是常规文件"},
		{name: "目录不可读", prepare: func(t *testing.T, dir, dataDir string) {
			if err := os.Chmod(dir, 0o000); err != nil {
				t.Fatalf("设置目录权限失败: %v", err)
			}
		}, restore: func(t *testing.T, dir, dataDir string) {
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatalf("恢复目录权限失败: %v", err)
			}
		}, keyword: "残留目录"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			imgTrashTestClosedApp(t, dataDir)
			dir := imgTrashTestQuarantineDir(t, dataDir)
			testCase.prepare(t, dir, dataDir)
			if testCase.restore != nil {
				// 先确认 New 失败，再恢复权限以便清理；cleanup 顺序后进先出。
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Logf("恢复目录权限失败: %v", err)
					}
				})
			}

			app, err := New(Config{DataDir: dataDir})
			if err == nil {
				_ = app.Close()
				t.Fatalf("无法解释的残留现场不应被静默清理：New 成功了")
			}
			if !strings.Contains(err.Error(), dir) {
				t.Fatalf("错误信息应指出残留目录 %s，实际: %v", dir, err)
			}
			if testCase.keyword != "" && !strings.Contains(err.Error(), testCase.keyword) {
				t.Fatalf("错误信息应包含 %q，实际: %v", testCase.keyword, err)
			}
			if testCase.restore != nil {
				testCase.restore(t, dir, dataDir)
			}
			// 现场必须原样保留（内容与条目都不能被删）。
			if _, statErr := os.Stat(dir); statErr != nil {
				t.Fatalf("残留目录被删除: %v", statErr)
			}
			if leftovers := imgTrashTestLeftovers(t, dataDir); len(leftovers) != 1 {
				t.Fatalf("残留目录丢失或数量变化: %v", leftovers)
			}
		})
	}
}
