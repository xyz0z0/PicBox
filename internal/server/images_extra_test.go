package server

// 标签候选 / 用量 / 批量 / URL 导入 / 单图元数据 的 HTTP 级回归。
// 全部使用真实临时目录 + 真实 SQLite（imgTestNewApp）与 httptest 请求。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- helpers ----

func imgExtraTestGet(t *testing.T, env *imgTestEnv, target string, auth imgTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(t, http.MethodGet, target, nil, "", auth)
}

func imgExtraTestBatch(t *testing.T, env *imgTestEnv, auth imgTestAuth, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化批量请求失败: %v", err)
	}
	return env.do(t, http.MethodPatch, "/api/images/batch", bytes.NewReader(payload), "application/json", auth)
}

func imgExtraTestStringList(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%s 不是数组: %v", key, body)
	}
	values := make([]string, 0, len(raw))
	for _, entry := range raw {
		value, ok := entry.(string)
		if !ok {
			t.Fatalf("%s 含非字符串元素: %v", key, raw)
		}
		values = append(values, value)
	}
	return values
}

// imgExtraTestRow 读取一张图片的关键状态，用来断言"拒绝请求没有副作用"。
func imgExtraTestRow(t *testing.T, env *imgTestEnv, imageID string) (deletedAt *string, isVisible int, updatedAt string, tags []string) {
	t.Helper()
	if err := env.app.db.QueryRow(
		`SELECT deletedAt, isVisible, updatedAt FROM "Image" WHERE id = ?`, imageID).
		Scan(&deletedAt, &isVisible, &updatedAt); err != nil {
		t.Fatalf("读取图片状态失败: %v", err)
	}
	return deletedAt, isVisible, updatedAt, imgTrashTestTags(t, env, imageID)
}

func imgExtraTestTags(t *testing.T, env *imgTestEnv, auth imgTestAuth) []string {
	t.Helper()
	recorder := imgExtraTestGet(t, env, "/api/images/tags", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("tags status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	tags := imgExtraTestStringList(t, imgTestJSON(t, recorder), "tags")
	sorted := append([]string{}, tags...)
	sort.Strings(sorted)
	if strings.Join(tags, ",") != strings.Join(sorted, ",") {
		t.Fatalf("tags 未升序: %v", tags)
	}
	return tags
}

// ---- 标签候选 ----

func TestImagesTagsEndpointScopesToOwnLibrary(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "tags-owner@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	env.imgTestUploadBytes(t, auth, "one.png", imgTestPNG(t, 20, 12), []string{"zeta", "alpha", "alpha"})
	env.imgTestUploadBytes(t, auth, "two.png", imgTestPNG(t, 20, 12), []string{"alpha", "beta"})
	trashedID, _ := imgTrashTestUpload(t, env, auth, "three.png", []string{"trashed-only"})
	imgTestTrash(t, env, auth, trashedID)

	otherID, otherCookie := env.addUser(t, "tags-other@example.com")
	if otherID <= 0 {
		t.Fatalf("测试账号 id 非法: %d", otherID)
	}
	imgTrashTestUpload(t, env, imgTestCookieAuth(otherCookie), "other.png", []string{"other-only"})

	got := imgExtraTestTags(t, env, auth)
	want := []string{"alpha", "beta", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tags = %v，期望 %v（回收站与他人的标签不能出现）", got, want)
	}

	other := imgExtraTestTags(t, env, imgTestCookieAuth(otherCookie))
	if strings.Join(other, ",") != "other-only" {
		t.Fatalf("另一个账号的 tags = %v", other)
	}

	// 原版允许 Bearer；未登录必须是 401 且不带标签数据。
	tagsBearer := imgExtraTestTags(t, env, imgTestBearerAuth(env.addAPIKey(t, 1)))
	if strings.Join(tagsBearer, ",") != strings.Join(want, ",") {
		t.Fatalf("Bearer 访问 tags = %v，期望 %v", tagsBearer, want)
	}
	unauthorized := imgExtraTestGet(t, env, "/api/images/tags", imgTestAuth{})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 tags status=%d", unauthorized.Code)
	}
	if strings.Contains(unauthorized.Body.String(), "alpha") {
		t.Fatalf("未登录响应泄露标签: %s", unauthorized.Body.String())
	}
}

// ---- 配额用量 ----

func TestImagesUsageTracksQuotaAndTrash(t *testing.T) {
	env := imgTestNewApp(t, Config{UserQuotaBytes: 1 << 20})
	cookie := env.registerAdmin(t, "usage@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	recorder := imgExtraTestGet(t, env, "/api/images/usage", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Fatalf("usage Cache-Control = %q，期望 no-store", cache)
	}
	body := imgTestJSON(t, recorder)
	if used, _ := body["usedBytes"].(float64); used != 0 {
		t.Fatalf("初始 usedBytes = %v", body["usedBytes"])
	}
	if quota, _ := body["quotaBytes"].(float64); int64(quota) != 1<<20 {
		t.Fatalf("quotaBytes = %v，期望 %d", body["quotaBytes"], 1<<20)
	}

	imageID, data := imgTrashTestUpload(t, env, auth, "usage.png", nil)
	afterUpload := imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))
	if used := int64(afterUpload["usedBytes"].(float64)); used != int64(len(data)) {
		t.Fatalf("上传后 usedBytes = %d，期望 %d", used, len(data))
	}

	// 回收站中的图片仍占磁盘，必须计入用量；彻底删除后才释放。
	imgTestTrash(t, env, auth, imageID)
	afterTrash := imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))
	if used := int64(afterTrash["usedBytes"].(float64)); used != int64(len(data)) {
		t.Fatalf("软删除后 usedBytes = %d，期望仍为 %d", used, len(data))
	}
	if recorder := imgTrashTestPurge(t, env, auth, imageID); recorder.Code != http.StatusOK {
		t.Fatalf("彻底删除失败: %s", recorder.Body.String())
	}
	afterPurge := imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))
	if used := int64(afterPurge["usedBytes"].(float64)); used != 0 {
		t.Fatalf("彻底删除后 usedBytes = %d，期望 0", used)
	}

	// 账号级配额覆盖优先于全局默认。
	overrideOwner := int64(3 << 20)
	env.setQuota(t, 1, overrideOwner)
	overridden := imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))
	if quota := int64(overridden["quotaBytes"].(float64)); quota != overrideOwner {
		t.Fatalf("账号配额覆盖后 quotaBytes = %d，期望 %d", quota, overrideOwner)
	}

	if recorder := imgExtraTestGet(t, env, "/api/images/usage", imgTestAuth{}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 usage status=%d", recorder.Code)
	}
}

// ---- 批量校验：任何非法输入都不产生副作用 ----

func TestImagesBatchValidationHasNoSideEffects(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-invalid@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, _ := imgTrashTestUpload(t, env, auth, "invalid.png", []string{"keep"})
	beforeDeleted, beforeVisible, beforeUpdated, beforeTags := imgExtraTestRow(t, env, imageID)

	longIDs := make([]string, 0, imagesBatchMaxIDs+1)
	for index := 0; index <= imagesBatchMaxIDs; index++ {
		longIDs = append(longIDs, uuid.NewString())
	}

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{name: "missing-ids", body: map[string]any{"action": "visibility", "visible": false}, status: 400, code: "INVALID_IDS"},
		{name: "empty-ids", body: map[string]any{"ids": []string{}, "action": "delete"}, status: 400, code: "INVALID_IDS"},
		{name: "ids-not-strings", body: map[string]any{"ids": []any{1, 2}, "action": "delete"}, status: 400, code: "INVALID_IDS"},
		{name: "blank-id", body: map[string]any{"ids": []string{"  "}, "action": "delete"}, status: 400, code: "INVALID_IDS"},
		{name: "ids-null", body: map[string]any{"ids": nil, "action": "delete"}, status: 400, code: "INVALID_IDS"},
		{name: "too-many", body: map[string]any{"ids": longIDs, "action": "delete"}, status: 400, code: "BATCH_TOO_LARGE"},
		{name: "unknown-action", body: map[string]any{"ids": []string{imageID}, "action": "explode"}, status: 400, code: "INVALID_ACTION"},
		{name: "missing-action", body: map[string]any{"ids": []string{imageID}}, status: 400, code: "INVALID_ACTION"},
		{name: "visibility-missing", body: map[string]any{"ids": []string{imageID}, "action": "visibility"}, status: 400, code: "INVALID_VISIBILITY"},
		{name: "visibility-null", body: map[string]any{"ids": []string{imageID}, "action": "visibility", "visible": nil}, status: 400, code: "INVALID_VISIBILITY"},
		{name: "visibility-string", body: map[string]any{"ids": []string{imageID}, "action": "visibility", "visible": "false"}, status: 400, code: "INVALID_VISIBILITY"},
		{name: "tag-missing", body: map[string]any{"ids": []string{imageID}, "action": "tag"}, status: 400, code: "INVALID_TAG"},
		{name: "tag-and-tags", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tag": "a", "tags": []string{"b"}}, status: 400, code: "INVALID_TAG"},
		{name: "tag-null", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tags": nil}, status: 400, code: "INVALID_TAG"},
		{name: "tag-control-char", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tags": []string{"bad\x01tag"}}, status: 400, code: "INVALID_TAG"},
		{name: "tag-too-many", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tags": imgExtraTestManyTags(21)}, status: 400, code: "INVALID_TAG"},
		{name: "tag-mode", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tags": []string{"a"}, "mode": "merge"}, status: 400, code: "INVALID_TAG_MODE"},
		{name: "legacy-tag-mode", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tag": "a", "mode": "add"}, status: 400, code: "INVALID_TAG_MODE"},
		{name: "tag-mode-null", body: map[string]any{"ids": []string{imageID}, "action": "tag", "tags": []string{"a"}, "mode": nil}, status: 400, code: "INVALID_TAG_MODE"},
		{name: "action-null", body: map[string]any{"ids": []string{imageID}, "action": nil}, status: 400, code: "INVALID_ACTION"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := imgExtraTestBatch(t, env, auth, testCase.body)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
			if code := imgTestErrorCode(t, recorder); code != testCase.code {
				t.Fatalf("error.code = %q，期望 %q", code, testCase.code)
			}
			afterDeleted, afterVisible, afterUpdated, afterTags := imgExtraTestRow(t, env, imageID)
			if (afterDeleted == nil) != (beforeDeleted == nil) || afterVisible != beforeVisible ||
				afterUpdated != beforeUpdated || strings.Join(afterTags, ",") != strings.Join(beforeTags, ",") {
				t.Fatalf("非法批量请求产生了副作用: deleted=%v visible=%d updated=%q tags=%v",
					afterDeleted, afterVisible, afterUpdated, afterTags)
			}
		})
	}
}

func imgExtraTestManyTags(count int) []string {
	tags := make([]string, 0, count)
	for index := 0; index < count; index++ {
		tags = append(tags, "tag-"+string(rune('a'+index%26))+"-"+string(rune('a'+index/26)))
	}
	return tags
}

// ---- 批量可见性 / 软删除 / 恢复 ----

func TestImagesBatchVisibilityDeleteRestoreScope(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-owner@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, _ := imgTrashTestUpload(t, env, auth, "one.png", nil)
	second, _ := imgTrashTestUpload(t, env, auth, "two.png", nil)
	third, _ := imgTrashTestUpload(t, env, auth, "three.png", nil)

	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids":     []string{first, second},
		"action":  "visibility",
		"visible": false,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量隐藏失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if count := int64(imgTestJSON(t, recorder)["updatedCount"].(float64)); count != 2 {
		t.Fatalf("updatedCount = %d，期望 2", count)
	}
	for _, id := range []string{first, second} {
		if _, visible, _, _ := imgExtraTestRow(t, env, id); visible != 0 {
			t.Fatalf("图片 %s 未被隐藏", id)
		}
	}
	if _, visible, _, _ := imgExtraTestRow(t, env, third); visible != 1 {
		t.Fatalf("第三张图片的可见性被误改")
	}

	if recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second, third}, "action": "delete",
	}); recorder.Code != http.StatusOK {
		t.Fatalf("批量软删除失败: %s", recorder.Body.String())
	}
	for _, id := range []string{first, second, third} {
		if deletedAt, _, _, _ := imgExtraTestRow(t, env, id); deletedAt == nil {
			t.Fatalf("图片 %s 未进入回收站", id)
		}
	}

	// 恢复只作用于回收站中的记录；不在回收站的 id 让整批失败且不产生部分成功。
	fourth, _ := imgTrashTestUpload(t, env, auth, "four.png", nil)
	before := map[string]string{}
	for _, id := range []string{first, fourth} {
		_, _, updatedAt, _ := imgExtraTestRow(t, env, id)
		before[id] = updatedAt
	}
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, fourth}, "action": "restore",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("混合作用域批量恢复 status=%d，期望 403 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "FORBIDDEN" {
		t.Fatalf("error.code = %q", code)
	}
	if deletedAt, _, updatedAt, _ := imgExtraTestRow(t, env, first); deletedAt == nil || updatedAt != before[first] {
		t.Fatalf("403 之后仍修改了图片 %s", first)
	}
	if deletedAt, _, updatedAt, _ := imgExtraTestRow(t, env, fourth); deletedAt != nil || updatedAt != before[fourth] {
		t.Fatalf("403 之后仍修改了图片 %s", fourth)
	}

	// 他人图片：整体 403，且本人图片状态不变。
	otherID := int64(0)
	var otherCookie *http.Cookie
	otherID, otherCookie = env.addUser(t, "batch-other@example.com")
	if otherID == 0 {
		t.Fatalf("创建第二个账号失败")
	}
	foreign, _ := imgTrashTestUpload(t, env, imgTestCookieAuth(otherCookie), "foreign.png", nil)
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, foreign}, "action": "restore",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("跨账号批量恢复 status=%d，期望 403", recorder.Code)
	}
	if deletedAt, _, _, _ := imgExtraTestRow(t, env, first); deletedAt == nil {
		t.Fatalf("跨账号 403 之后本人图片被恢复")
	}
	if deletedAt, _, _, _ := imgExtraTestRow(t, env, foreign); deletedAt != nil {
		t.Fatalf("他人图片被修改")
	}

	// 只恢复本人回收站记录：成功路径。
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second, third}, "action": "restore",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量恢复失败: %s", recorder.Body.String())
	}
	for _, id := range []string{first, second, third} {
		if deletedAt, _, _, _ := imgExtraTestRow(t, env, id); deletedAt != nil {
			t.Fatalf("图片 %s 未被恢复", id)
		}
	}

	// 未登录 / 无效图片 id：不允许任何写入。
	if recorder := imgExtraTestBatch(t, env, imgTestAuth{}, map[string]any{
		"ids": []string{first}, "action": "delete",
	}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未登录批量 status=%d", recorder.Code)
	}
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{uuid.NewString()}, "action": "delete",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("不存在 id 的批量 status=%d，期望 403", recorder.Code)
	}
}

// ---- 批量标签 add / remove / replace ----

func TestImagesBatchTagModesAndLimit(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-tags@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, _ := imgTrashTestUpload(t, env, auth, "a.png", []string{"a", "b"})
	second, _ := imgTrashTestUpload(t, env, auth, "b.png", []string{"b", "c"})

	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "tag", "tags": []string{"b", "d"}, "mode": "add",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量加标签失败: %s", recorder.Body.String())
	}
	if tags := imgTrashTestTags(t, env, first); strings.Join(tags, ",") != "a,b,d" {
		t.Fatalf("add 后 first tags = %v，期望 a,b,d", tags)
	}
	if tags := imgTrashTestTags(t, env, second); strings.Join(tags, ",") != "b,c,d" {
		t.Fatalf("add 后 second tags = %v，期望 b,c,d", tags)
	}

	if recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "tag", "tags": []string{"b", "d"}, "mode": "remove",
	}); recorder.Code != http.StatusOK {
		t.Fatalf("批量移除标签失败: %s", recorder.Body.String())
	}
	if tags := imgTrashTestTags(t, env, first); strings.Join(tags, ",") != "a" {
		t.Fatalf("remove 后 first tags = %v，期望 a", tags)
	}
	if tags := imgTrashTestTags(t, env, second); strings.Join(tags, ",") != "c" {
		t.Fatalf("remove 后 second tags = %v，期望 c", tags)
	}

	// 旧字段 tag（标量）只支持 replace。
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first}, "action": "tag", "tag": "solo",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("legacy tag 替换失败: %s", recorder.Body.String())
	}
	if tags := imgTrashTestTags(t, env, first); strings.Join(tags, ",") != "solo" {
		t.Fatalf("replace 后 tags = %v，期望 solo", tags)
	}

	// 替换为空数组表示清空标签（原版 replace 语义），不是"忽略"。
	if recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first}, "action": "tag", "tags": []string{},
	}); recorder.Code != http.StatusOK {
		t.Fatalf("清空标签失败: %s", recorder.Body.String())
	}
	if tags := imgTrashTestTags(t, env, first); len(tags) != 0 {
		t.Fatalf("清空后 tags = %v", tags)
	}

	// add 突破 20 个标签上限时必须整体 400，且不写入任何标签。
	full := imgExtraTestManyTags(imagesMaxTags)
	limitID, _ := imgTrashTestUpload(t, env, auth, "full.png", full)
	_, _, beforeUpdated, beforeTags := imgExtraTestRow(t, env, limitID)
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{limitID}, "action": "tag", "tags": []string{"overflow"}, "mode": "add",
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("超上限 add status=%d，期望 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "INVALID_TAG" {
		t.Fatalf("error.code = %q", code)
	}
	_, _, afterUpdated, afterTags := imgExtraTestRow(t, env, limitID)
	if afterUpdated != beforeUpdated || strings.Join(afterTags, ",") != strings.Join(beforeTags, ",") {
		t.Fatalf("超上限 add 产生了副作用: %v", afterTags)
	}

	// 标签批量动作只作用于未删除图片：回收站记录让整批 403。
	imgTestTrash(t, env, auth, second)
	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "tag", "tags": []string{"x"}, "mode": "replace",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("含回收站记录的标签批量 status=%d，期望 403", recorder.Code)
	}
}

// ---- 批量彻底删除 ----

func TestImagesBatchPurgeCountsRealRemovals(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-purge@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, firstData := imgTrashTestUpload(t, env, auth, "purge-a.png", nil)
	second, secondData := imgTrashTestUpload(t, env, auth, "purge-b.png", nil)
	keep, _ := imgTrashTestUpload(t, env, auth, "keep.png", nil)

	originalAbs, thumbAbs := imgTrashTestPaths(t, env, first)
	imgTestTrash(t, env, auth, first)
	imgTestTrash(t, env, auth, second)

	// 混入未软删除的图片：整批 403，且没有任何图片被删除。
	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second, keep}, "action": "purge",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("混合作用域批量彻底删除 status=%d，期望 403 body=%s", recorder.Code, recorder.Body.String())
	}
	var rowsAfterForbidden int
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image"`).Scan(&rowsAfterForbidden); err != nil {
		t.Fatalf("数据库不可用: %v", err)
	}
	if imgTrashTestFileMissing(t, originalAbs) {
		t.Fatalf("403 之后仍删除了文件")
	}

	recorder = imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "purge",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量彻底删除失败: %s", recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	if count := int64(body["updatedCount"].(float64)); count != 2 {
		t.Fatalf("updatedCount = %d，期望 2 body=%v", count, body)
	}
	if pending, ok := body["cleanupPending"]; ok {
		if pending.(float64) != 0 {
			t.Fatalf("cleanupPending = %v，期望 0（本用例没有制造清理失败）", pending)
		}
	}
	for _, id := range []string{first, second} {
		var count int
		if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image" WHERE id = ?`, id).Scan(&count); err != nil {
			t.Fatalf("查询图片失败: %v", err)
		}
		if count != 0 {
			t.Fatalf("图片 %s 的记录未被删除", id)
		}
	}
	if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
		t.Fatalf("批量彻底删除后原图/缩略图仍存在")
	}
	if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
		t.Fatalf("批量彻底删除留下暂存残留: %v", leftovers)
	}

	// 配额只统计剩下的图片：删除两张后用量等于 keep 的大小。
	used := imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))["usedBytes"].(float64)
	var keepSize int
	if err := env.app.db.QueryRow(`SELECT size FROM "Image" WHERE id = ?`, keep).Scan(&keepSize); err != nil {
		t.Fatalf("读取保留图片大小失败: %v", err)
	}
	if int64(used) != int64(keepSize) {
		t.Fatalf("批量彻底删除后 usedBytes = %v，期望 %d", used, keepSize)
	}
	if len(firstData) == 0 || len(secondData) == 0 {
		t.Fatalf("测试数据为空")
	}
}

func TestImagesBatchPurgeReportsRemovedIDs(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-purge-ids@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, _ := imgTrashTestUpload(t, env, auth, "ids-a.png", nil)
	second, _ := imgTrashTestUpload(t, env, auth, "ids-b.png", nil)
	imgTestTrash(t, env, auth, first)
	imgTestTrash(t, env, auth, second)

	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "purge",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量彻底删除失败: %s", recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	if count := int64(body["updatedCount"].(float64)); count != 2 {
		t.Fatalf("updatedCount = %d，期望 2", count)
	}
	// updatedIds 是 UI 精确移除的依据：必须是真正删掉行的 id，且保持请求顺序。
	if ids := imgExtraTestStringList(t, body, "updatedIds"); !slices.Equal(ids, []string{first, second}) {
		t.Fatalf("updatedIds = %v，期望 %v", ids, []string{first, second})
	}
	for _, id := range []string{first, second} {
		var count int
		if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image" WHERE id = ?`, id).Scan(&count); err != nil {
			t.Fatalf("查询图片失败: %v", err)
		}
		if count != 0 {
			t.Fatalf("图片 %s 的记录未被删除", id)
		}
	}
}

func TestImagesBatchPurgeReportsOnlyDeletedIDsOnPartialFailure(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-purge-partial@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	blocked, _ := imgTrashTestUpload(t, env, auth, "partial-blocked.png", nil)
	allowed, _ := imgTrashTestUpload(t, env, auth, "partial-allowed.png", nil)
	imgTestTrash(t, env, auth, blocked)
	imgTestTrash(t, env, auth, allowed)

	// 只阻止其中一张的 DELETE：另一张必须仍然成功，失败的那张不能被报告为已删除。
	trigger := fmt.Sprintf(`CREATE TRIGGER extra_test_block_purge BEFORE DELETE ON "Image"
		WHEN OLD.id = '%s' BEGIN SELECT RAISE(ABORT, 'forced purge failure'); END`, blocked)
	if _, err := env.app.db.Exec(trigger); err != nil {
		t.Fatalf("创建失败触发器失败: %v", err)
	}
	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{blocked, allowed}, "action": "purge",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("部分失败仍是 200 且带真实计数，实际 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	if count := int64(body["updatedCount"].(float64)); count != 1 {
		t.Fatalf("updatedCount = %d，期望 1 body=%v", count, body)
	}
	if failed := int64(body["failedCount"].(float64)); failed != 1 {
		t.Fatalf("failedCount = %d，期望 1 body=%v", failed, body)
	}
	if ids := imgExtraTestStringList(t, body, "updatedIds"); !slices.Equal(ids, []string{allowed}) {
		t.Fatalf("updatedIds = %v，期望仅 [%s]", ids, allowed)
	}
	var blockedRows int
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image" WHERE id = ?`, blocked).Scan(&blockedRows); err != nil {
		t.Fatalf("查询失败图片失败: %v", err)
	}
	if blockedRows != 1 {
		t.Fatalf("失败的图片必须保留数据库行，实际 count=%d", blockedRows)
	}
	if recorderBody := recorder.Body.String(); strings.Contains(recorderBody, `"cleanupPending"`) {
		t.Fatalf("本用例没有物理清理失败，cleanupPending 不应出现: %s", recorderBody)
	}

	// 失败项仍在回收站内，移除触发器后再次提交时必须真实删除（UI 只重试失败项）。
	if _, err := env.app.db.Exec(`DROP TRIGGER extra_test_block_purge`); err != nil {
		t.Fatalf("移除失败触发器失败: %v", err)
	}
	retry := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{blocked}, "action": "purge",
	})
	if retry.Code != http.StatusOK {
		t.Fatalf("重试失败: %s", retry.Body.String())
	}
	retryBody := imgTestJSON(t, retry)
	if ids := imgExtraTestStringList(t, retryBody, "updatedIds"); !slices.Equal(ids, []string{blocked}) {
		t.Fatalf("重试 updatedIds = %v，期望 [%s]", ids, blocked)
	}
}

func TestImagesBatchPurgeReportsEmptyUpdatedIDsWhenAllFail(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-purge-allfail@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, _ := imgTrashTestUpload(t, env, auth, "allfail-a.png", nil)
	second, _ := imgTrashTestUpload(t, env, auth, "allfail-b.png", nil)
	imgTestTrash(t, env, auth, first)
	imgTestTrash(t, env, auth, second)

	trigger := fmt.Sprintf(`CREATE TRIGGER extra_test_block_all_purge BEFORE DELETE ON "Image"
		WHEN OLD.id IN ('%s', '%s') BEGIN SELECT RAISE(ABORT, 'forced purge failure'); END`, first, second)
	if _, err := env.app.db.Exec(trigger); err != nil {
		t.Fatalf("创建失败触发器失败: %v", err)
	}
	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "purge",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("全部失败仍应 200 并报告失败计数: %d %s", recorder.Code, recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	if count := int64(body["updatedCount"].(float64)); count != 0 {
		t.Fatalf("updatedCount = %d，期望 0", count)
	}
	if failed := int64(body["failedCount"].(float64)); failed != 2 {
		t.Fatalf("failedCount = %d，期望 2", failed)
	}
	// 空集合也必须显式返回（[] 而不是 null），否则前端会退回「全部移除」的旧行为。
	raw, present := body["updatedIds"]
	if !present {
		t.Fatalf("updatedIds 字段必须存在: %v", body)
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("updatedIds = %v，期望空数组", raw)
	}
}

func TestImagesBatchPurgeResultSetIncludesCleanupPending(t *testing.T) {
	forced := errors.New("forced purge failure")
	response := imagesBatchPurgeResults([]string{"a", "b", "c", "d"}, func(id string) (bool, error) {
		switch id {
		case "b":
			return true, nil // 行已删除、物理清理未完成
		case "d":
			return false, forced // 行未删除
		default:
			return false, nil
		}
	})

	if response.UpdatedCount != 3 || response.CleanupPending != 1 || response.FailedCount != 1 {
		t.Fatalf("计数错误: %+v", response)
	}
	if !slices.Equal(response.UpdatedIDs, []string{"a", "b", "c"}) {
		t.Fatalf("updatedIds = %v，期望 [a b c]（含 cleanupPending 的 b、不含失败的 d）", response.UpdatedIDs)
	}
}

// ---- URL 导入：SSRF 与输入校验 ----

func TestImagesRemoteAddressPolicyRejectsInternalTargets(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "0.0.0.0", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "198.18.0.1", "203.0.113.9", "224.0.0.1",
		"240.0.0.1", "::1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1", "::ffff:127.0.0.1",
	}
	for _, raw := range blocked {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", raw, err)
		}
		if imagesRemoteAddressAllowed(address) {
			t.Fatalf("%q 被错误地当成公网地址", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", raw, err)
		}
		if !imagesRemoteAddressAllowed(address) {
			t.Fatalf("%q 被错误地拒绝", raw)
		}
	}

	// 生产客户端的拨号器必须自己拒绝环回/私网，不能只靠上层判断。
	client := imagesRemoteNewClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatalf("生产客户端缺少自定义拨号器")
	}
	for _, target := range []string{"127.0.0.1:80", "[::1]:80", "169.254.169.254:80", "10.0.0.1:80"} {
		if _, err := transport.DialContext(context.Background(), "tcp", target); err == nil {
			t.Fatalf("拨号器允许了内部地址 %s", target)
		}
	}
}

func TestImagesFromURLRejectsUnsafeAndInvalidInput(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "from-url@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{name: "missing-url", body: map[string]any{"tags": []string{}}, status: 400, code: "INVALID_URL"},
		{name: "null-url", body: map[string]any{"url": nil}, status: 400, code: "INVALID_URL"},
		{name: "empty-url", body: map[string]any{"url": "   "}, status: 400, code: "INVALID_URL"},
		{name: "file-url", body: map[string]any{"url": "file:///etc/passwd"}, status: 400, code: "INVALID_URL"},
		{name: "ftp-url", body: map[string]any{"url": "ftp://example.com/a.png"}, status: 400, code: "INVALID_URL"},
		{name: "userinfo-url", body: map[string]any{"url": "http://user:pass@example.com/a.png"}, status: 400, code: "INVALID_URL"},
		{name: "too-long-url", body: map[string]any{"url": "http://example.com/" + strings.Repeat("a", 9000)}, status: 400, code: "INVALID_URL"},
		{name: "tags-null", body: map[string]any{"url": "http://example.com/a.png", "tags": nil}, status: 400, code: "INVALID_TAG"},
		{name: "tags-not-array", body: map[string]any{"url": "http://example.com/a.png", "tags": "x"}, status: 400, code: "INVALID_TAG"},
		{name: "localhost", body: map[string]any{"url": "http://localhost:8080/a.png"}, status: 400, code: "UNSAFE_URL"},
		{name: "loopback", body: map[string]any{"url": "http://127.0.0.1:8080/a.png"}, status: 400, code: "UNSAFE_URL"},
		{name: "private", body: map[string]any{"url": "http://10.1.2.3/a.png"}, status: 400, code: "UNSAFE_URL"},
		{name: "link-local-metadata", body: map[string]any{"url": "http://169.254.169.254/latest/meta-data/"}, status: 400, code: "UNSAFE_URL"},
		{name: "ipv6-loopback", body: map[string]any{"url": "http://[::1]:8080/a.png"}, status: 400, code: "UNSAFE_URL"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload, err := json.Marshal(testCase.body)
			if err != nil {
				t.Fatalf("序列化请求失败: %v", err)
			}
			recorder := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(payload), "application/json", auth)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
			if code := imgTestErrorCode(t, recorder); code != testCase.code {
				t.Fatalf("error.code = %q，期望 %q", code, testCase.code)
			}
			if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
				t.Fatalf("被拒绝的导入留下暂存文件: %v", leftovers)
			}
		})
	}

	// 配额已满时不得先下载：这里用一个一定不存在的公网地址，只有顺序正确才会得到配额错误。
	imgTrashTestUpload(t, env, auth, "fill.png", nil)
	env.setQuota(t, 1, 1)
	payload, err := json.Marshal(map[string]any{"url": "https://example.com/never-fetched.png"})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	recorder := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(payload), "application/json", auth)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("配额已满 status=%d，期望 413 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "USER_QUOTA_EXCEEDED" {
		t.Fatalf("error.code = %q", code)
	}

	// 单处理槽被占用时 429，不排队。
	env.setQuota(t, 1, 0)
	env.app.uploadSlots <- struct{}{}
	recorder = env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(payload), "application/json", auth)
	<-env.app.uploadSlots
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("上传槽占用时 status=%d，期望 429", recorder.Code)
	}
	if code := imgTestErrorCode(t, recorder); code != "UPLOAD_BUSY" {
		t.Fatalf("error.code = %q", code)
	}
}

// ---- URL 导入：受控下载 ----

// imgExtraTestRemoteClient 让下载路径访问环回 httptest 服务（生产路径的 SSRF 校验在
// TestImagesRemoteAddressPolicyRejectsInternalTargets 中单独断言）。
func imgExtraTestRemoteClient(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := imagesRemoteClientFactory
	serverAddr := strings.TrimPrefix(server.URL, "http://")
	imagesRemoteClientFactory = func() *http.Client {
		return &http.Client{
			Transport: &http.Transport{
				// 测试里把所有主机名都指向 httptest 服务，模拟"公网图片服务器"。
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", serverAddr)
				},
			},
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= imagesRemoteMaxRedirects {
					return errImagesRemoteRedirectTooMany
				}
				return nil
			},
		}
	}
	t.Cleanup(func() { imagesRemoteClientFactory = previous })
	return server
}

func TestImagesFromURLImportsControlledPublicImage(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "from-url-ok@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	image := imgTestPNG(t, 32, 24)

	mux := http.NewServeMux()
	mux.HandleFunc("/photo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/photo.png", http.StatusFound)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/too-big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write(image)
	})
	mux.HandleFunc("/not-an-image", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not an image"))
	})
	mux.HandleFunc("/encoded", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(image)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	server := imgExtraTestRemoteClient(t, mux)

	payload, err := json.Marshal(map[string]any{"url": server.URL + "/photo.png", "tags": []string{"imported", "remote"}})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	recorder := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(payload), "application/json", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	imageID := imgTestMustImageID(t, body)
	if tags := imgExtraTestStringList(t, body, "tags"); strings.Join(tags, ",") != "imported,remote" {
		t.Fatalf("导入响应 tags = %v", tags)
	}

	var relPath, originalName, mimeType string
	var size int64
	if err := env.app.db.QueryRow(
		`SELECT originalPath, originalName, originalMimeType, size FROM "Image" WHERE id = ?`, imageID).
		Scan(&relPath, &originalName, &mimeType, &size); err != nil {
		t.Fatalf("读取导入图片失败: %v", err)
	}
	if originalName != "photo.png" || mimeType != "image/png" || size != int64(len(image)) {
		t.Fatalf("导入元数据异常: name=%q mime=%q size=%d", originalName, mimeType, size)
	}
	absolute, err := env.app.imagesSafePath(relPath)
	if err != nil {
		t.Fatalf("解析导入路径失败: %v", err)
	}
	stored, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatalf("读取导入文件失败: %v", err)
	}
	sum := sha256.Sum256(stored)
	want := sha256.Sum256(image)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(want[:]) {
		t.Fatalf("导入字节与原图不一致")
	}
	if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
		t.Fatalf("成功导入留下暂存文件: %v", leftovers)
	}

	// 重定向链：一跳允许，自环超过 3 次拒绝。
	redirectPayload, err := json.Marshal(map[string]any{"url": server.URL + "/redirect"})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	redirected := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(redirectPayload), "application/json", auth)
	if redirected.Code != http.StatusOK {
		t.Fatalf("单跳重定向导入失败: status=%d body=%s", redirected.Code, redirected.Body.String())
	}
	loopPayload, err := json.Marshal(map[string]any{"url": server.URL + "/loop"})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	loop := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(loopPayload), "application/json", auth)
	if loop.Code != http.StatusBadRequest {
		t.Fatalf("重定向过多 status=%d，期望 400 body=%s", loop.Code, loop.Body.String())
	}
	if code := imgTestErrorCode(t, loop); code != "INVALID_REDIRECT" {
		t.Fatalf("error.code = %q，期望 INVALID_REDIRECT", code)
	}

	for _, testCase := range []struct {
		path   string
		status int
		code   string
	}{
		{path: "/missing", status: http.StatusBadRequest, code: "DOWNLOAD_FAILED"},
		{path: "/too-big", status: http.StatusRequestEntityTooLarge, code: "FILE_TOO_LARGE"},
		{path: "/not-an-image", status: http.StatusBadRequest, code: "INVALID_FILE_TYPE"},
		{path: "/encoded", status: http.StatusBadRequest, code: "INVALID_ENCODING"},
	} {
		body, err := json.Marshal(map[string]any{"url": server.URL + testCase.path})
		if err != nil {
			t.Fatalf("序列化请求失败: %v", err)
		}
		recorder := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(body), "application/json", auth)
		if recorder.Code != testCase.status {
			t.Fatalf("%s status=%d，期望 %d body=%s", testCase.path, recorder.Code, testCase.status, recorder.Body.String())
		}
		if code := imgTestErrorCode(t, recorder); code != testCase.code {
			t.Fatalf("%s error.code = %q，期望 %q", testCase.path, code, testCase.code)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("%s 留下暂存文件: %v", testCase.path, leftovers)
		}
	}
}

func TestImagesFromURLImportsAnimatedWebP(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "from-url-animated@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	animated := imgTestAnimatedWebP(t, 64, 48, 2)

	mux := http.NewServeMux()
	mux.HandleFunc("/animated.webp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(animated)
	})
	server := imgExtraTestRemoteClient(t, mux)

	payload, err := json.Marshal(map[string]any{"url": server.URL + "/animated.webp"})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	recorder := env.do(t, http.MethodPost, "/api/images/from-url", bytes.NewReader(payload), "application/json", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("导入动画 WebP 失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := imgTestJSON(t, recorder)
	imageID := imgTestMustImageID(t, body)

	var relPath, mimeType string
	var size int64
	if err := env.app.db.QueryRow(
		`SELECT originalPath, originalMimeType, size FROM "Image" WHERE id = ?`, imageID).
		Scan(&relPath, &mimeType, &size); err != nil {
		t.Fatalf("读取导入的动画图片失败: %v", err)
	}
	if mimeType != "image/webp" || size != int64(len(animated)) {
		t.Fatalf("导入元数据异常: mime=%q size=%d，期望 image/webp %d", mimeType, size, len(animated))
	}
	absolute, err := env.app.imagesSafePath(relPath)
	if err != nil {
		t.Fatalf("解析导入路径失败: %v", err)
	}
	stored, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatalf("读取导入文件失败: %v", err)
	}
	if !bytes.Equal(stored, animated) {
		t.Fatalf("URL 导入的动画 WebP 字节被改写: %d -> %d", len(animated), len(stored))
	}
	if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
		t.Fatalf("成功导入动画留下暂存文件: %v", leftovers)
	}

	// 缩略图仍取首帧、画布尺寸；URL 导入与 multipart 共用同一条路径。
	thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
	if thumb.Code != http.StatusOK {
		t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
	}
	decoded, format, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	if format != "webp" {
		t.Fatalf("缩略图格式 = %q，期望 webp", format)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 64 || bounds.Dy() != 48 {
		t.Fatalf("缩略图尺寸 = %dx%d，期望画布 64x48", bounds.Dx(), bounds.Dy())
	}
}

// ---- 单图元数据 ----

func TestImagesMetaVisibility(t *testing.T) {
	env := imgTestNewApp(t, Config{BaseURL: "https://images.example.com"})
	cookie := env.registerAdmin(t, "meta-owner@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	ownerID := int64(1)
	apiKey := env.addAPIKey(t, ownerID)
	_, otherCookie := env.addUser(t, "meta-other@example.com")

	imageID, data := imgTrashTestUpload(t, env, auth, "detail.png", []string{"blue", "alpha"})

	recorder := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", imgTestAuth{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("可见图片元数据 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	image, ok := imgTestJSON(t, recorder)["image"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 image 对象: %s", recorder.Body.String())
	}
	if image["id"] != imageID || image["originalName"] != "detail.png" || image["originalMimeType"] != "image/png" {
		t.Fatalf("元数据字段异常: %v", image)
	}
	if int64(image["size"].(float64)) != int64(len(data)) {
		t.Fatalf("size = %v，期望 %d", image["size"], len(data))
	}
	if image["width"].(float64) != 24 || image["height"].(float64) != 18 {
		t.Fatalf("宽高异常: %v x %v", image["width"], image["height"])
	}
	if image["isVisible"] != true {
		t.Fatalf("isVisible = %v", image["isVisible"])
	}
	if tags := imgExtraTestStringList(t, image, "tags"); strings.Join(tags, ",") != "alpha,blue" {
		t.Fatalf("tags = %v", tags)
	}
	if image["url"] != "https://images.example.com/api/images/"+imageID+"?variant=original" {
		t.Fatalf("url = %v", image["url"])
	}
	if image["pageUrl"] != "https://images.example.com/images/"+imageID {
		t.Fatalf("pageUrl = %v", image["pageUrl"])
	}
	if image["thumbUrl"] != "https://images.example.com/api/images/"+imageID+"?variant=thumb" {
		t.Fatalf("thumbUrl = %v", image["thumbUrl"])
	}
	if _, exists := image["createdAt"]; !exists {
		t.Fatalf("缺少 createdAt")
	}

	// 隐藏图片：只有上传者本人的 Cookie 能读元数据，其他人（含 API Key）一律 404。
	imgTestSetHidden(t, env, auth, imageID, false)
	for name, requester := range map[string]imgTestAuth{
		"anonymous":     {},
		"other-cookie":  imgTestCookieAuth(otherCookie),
		"owner-api-key": imgTestBearerAuth(apiKey),
	} {
		hidden := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", requester)
		if hidden.Code != http.StatusNotFound {
			t.Fatalf("%s 读取隐藏图片元数据 status=%d，期望 404", name, hidden.Code)
		}
		if strings.Contains(hidden.Body.String(), "detail.png") {
			t.Fatalf("%s 的 404 泄露了图片信息", name)
		}
	}
	owner := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", auth)
	if owner.Code != http.StatusOK {
		t.Fatalf("上传者读取隐藏图片元数据 status=%d body=%s", owner.Code, owner.Body.String())
	}
	if imgTestJSON(t, owner)["image"].(map[string]any)["isVisible"] != false {
		t.Fatalf("上传者读到的 isVisible 应为 false")
	}

	// 软删除后对所有人 404（包括上传者）。
	imgTestTrash(t, env, auth, imageID)
	if trashed := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", auth); trashed.Code != http.StatusNotFound {
		t.Fatalf("回收站图片元数据 status=%d，期望 404", trashed.Code)
	}
	imgTrashTestRestore(t, env, auth, imageID)
	if restored := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", auth); restored.Code != http.StatusOK {
		t.Fatalf("恢复后元数据 status=%d，期望 200", restored.Code)
	}

	// 管理员下架（deletedAt 非空 + deletedByAdmin=1）同样 404。
	if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedAt = ?, deletedByAdmin = 1 WHERE id = ?`, imagesNow(), imageID); err != nil {
		t.Fatalf("模拟管理员下架失败: %v", err)
	}
	if removed := imgExtraTestGet(t, env, "/api/images/"+imageID+"/meta", auth); removed.Code != http.StatusNotFound {
		t.Fatalf("管理员下架图片元数据 status=%d，期望 404", removed.Code)
	}

	if missing := imgExtraTestGet(t, env, "/api/images/"+uuid.NewString()+"/meta", auth); missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的图片元数据 status=%d，期望 404", missing.Code)
	}
	// 多段路径不会被误当成 meta。
	if malformed := imgExtraTestGet(t, env, "/api/images/x/y/meta", auth); malformed.Code == http.StatusOK {
		t.Fatalf("非法 meta 路径返回了 200")
	}
}

// ---- 路由注册与请求方法矩阵 ----
//
// 这些断言直接描述真实 ServeMux 拓扑（单一 mux、无前置层）：
// 字面量路径不会被当成 {id}、GET 模式同时服务 HEAD、方法不支持时的错误来自
// images.go 的 /api/images/ 通配 handler（因此是 404/501 而不是 405 ——
// 405 只在没有任何模式匹配路径时由 ServeMux 产生，而通配 handler 匹配所有方法）。

func imgRouteTestExpect(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, status, recorder.Body.String())
	}
	if code == "" {
		return
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q，期望 JSON", contentType)
	}
	if got := imgTestErrorCode(t, recorder); got != code {
		t.Fatalf("error.code = %q，期望 %q", got, code)
	}
}

func imgRouteTestJSONBody(t *testing.T, env *imgTestEnv, method, target string, auth imgTestAuth, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	return env.do(t, method, target, bytes.NewReader(payload), "application/json", auth)
}

func TestImagesRouteMethodsAndHeadParity(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "routes@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imageID, imageData := imgTrashTestUpload(t, env, auth, "routes.png", []string{"route"})

	// ---- 字面量 GET 补全路由：GET 与 HEAD 都必须命中真实 handler ----
	for _, target := range []string{"/api/images/tags", "/api/images/usage"} {
		get := imgExtraTestGet(t, env, target, auth)
		if get.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", target, get.Code, get.Body.String())
		}
		if contentType := get.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
			t.Fatalf("GET %s Content-Type = %q", target, contentType)
		}
		head := env.do(t, http.MethodHead, target, nil, "", auth)
		if head.Code != http.StatusOK {
			t.Fatalf("HEAD %s status=%d body=%s（GET 模式必须同时匹配 HEAD）", target, head.Code, head.Body.String())
		}
		if contentType := head.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
			t.Fatalf("HEAD %s Content-Type = %q", target, contentType)
		}
		// 未登录时同样是这两个 handler，而不是被当成 {id} 落到 404。
		anonymous := env.do(t, http.MethodGet, target, nil, "", imgTestAuth{})
		imgRouteTestExpect(t, anonymous, http.StatusUnauthorized, "UNAUTHORIZED")
	}

	// 元数据：可见图片对未登录调用者公开（原版详情页语义），HEAD 同样命中该 handler。
	metaTarget := "/api/images/" + imageID + "/meta"
	if anonymous := imgExtraTestGet(t, env, metaTarget, imgTestAuth{}); anonymous.Code != http.StatusOK {
		t.Fatalf("可见图片元数据对未登录调用者应 200: %d body=%s", anonymous.Code, anonymous.Body.String())
	}
	if headMeta := env.do(t, http.MethodHead, metaTarget, nil, "", auth); headMeta.Code != http.StatusOK {
		t.Fatalf("HEAD 元数据 status=%d body=%s", headMeta.Code, headMeta.Body.String())
	}

	// 导出：HEAD 不写 body、不声明预约；GET 才产出归档。
	headExport := env.do(t, http.MethodHead, "/api/images/export?scope=all", nil, "", auth)
	if headExport.Code != http.StatusOK {
		t.Fatalf("HEAD /api/images/export status=%d body=%s", headExport.Code, headExport.Body.String())
	}
	if headExport.Body.Len() != 0 {
		t.Fatalf("HEAD 导出不应有响应体: %d 字节", headExport.Body.Len())
	}
	if count := headExport.Header().Get("X-Export-Count"); count != "1" {
		t.Fatalf("HEAD 导出 X-Export-Count = %q，期望 1", count)
	}

	// ---- 动作路由 ----
	batch := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{imageID}, "action": "visibility", "visible": true,
	})
	if batch.Code != http.StatusOK {
		t.Fatalf("PATCH /api/images/batch status=%d body=%s", batch.Code, batch.Body.String())
	}
	fromURL := imgRouteTestJSONBody(t, env, http.MethodPost, "/api/images/from-url", auth, map[string]any{"url": ""})
	imgRouteTestExpect(t, fromURL, http.StatusBadRequest, "INVALID_URL")

	// ---- 方法不支持：由 /api/images/ 通配 handler 决定（不是 405）----
	methodCases := []struct {
		method string
		target string
		status int
		code   string
	}{
		{method: http.MethodPost, target: "/api/images/tags", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodDelete, target: "/api/images/usage", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodPost, target: "/api/images/export", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodPut, target: "/api/images/export", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodPost, target: "/api/images/batch", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodGet, target: "/api/images/from-url", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodPost, target: "/api/images/" + imageID + "/meta", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodPost, target: "/api/images/url", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodGet, target: "/api/images/" + imageID + "/unknown", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodGet, target: "/api/nope", status: http.StatusNotFound, code: "NOT_FOUND"},
	}
	for _, testCase := range methodCases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			// 未登录也必须是同一个状态/错误码：路由决定行为，不是认证阶段。
			recorder := env.do(t, testCase.method, testCase.target, nil, "", auth)
			imgRouteTestExpect(t, recorder, testCase.status, testCase.code)
		})
	}

	// GET /api/images/batch 会被当成 {id}="batch"：与真实不存在的 id 行为一致。
	imgRouteTestExpect(t, env.do(t, http.MethodGet, "/api/images/batch", nil, "", auth), http.StatusNotFound, "NOT_FOUND")

	// ---- 既有单图路由：删除多余的 HEAD 注册后，HEAD 仍由 GET 模式服务 ----
	headFile := env.do(t, http.MethodHead, "/api/images/"+imageID+"?variant=original", nil, "", auth)
	if headFile.Code != http.StatusOK {
		t.Fatalf("HEAD 原图 status=%d body=%s", headFile.Code, headFile.Body.String())
	}
	if headFile.Body.Len() != 0 {
		t.Fatalf("HEAD 原图不应有响应体: %d 字节", headFile.Body.Len())
	}
	if contentType := headFile.Header().Get("Content-Type"); contentType != "image/png" {
		t.Fatalf("HEAD 原图 Content-Type = %q", contentType)
	}
	if length := headFile.Header().Get("Content-Length"); length != strconv.Itoa(len(imageData)) {
		t.Fatalf("HEAD 原图 Content-Length = %q，期望 %d", length, len(imageData))
	}
	etag := headFile.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("HEAD 原图缺少 ETag")
	}
	notModified := httptest.NewRequest(http.MethodHead, "/api/images/"+imageID+"?variant=original", nil)
	notModified.AddCookie(cookie)
	notModified.Header.Set("If-None-Match", etag)
	conditional := httptest.NewRecorder()
	env.handler.ServeHTTP(conditional, notModified)
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("HEAD 条件请求 status=%d，期望 304", conditional.Code)
	}

	// 列表、页面与健康检查在单 mux 下仍然正常。
	if list := env.do(t, http.MethodGet, "/api/images?mine=true", nil, "", auth); list.Code != http.StatusOK {
		t.Fatalf("图片列表 status=%d body=%s", list.Code, list.Body.String())
	}
	if health := env.do(t, http.MethodGet, "/api/health", nil, "", imgTestAuth{}); health.Code != http.StatusOK {
		t.Fatalf("健康检查 status=%d", health.Code)
	}
	for _, page := range []string{"/", "/dashboard"} {
		if recorder := env.do(t, http.MethodGet, page, nil, "", imgTestAuth{}); recorder.Code != http.StatusOK {
			t.Fatalf("页面 %s status=%d", page, recorder.Code)
		}
	}

	// 最后再跑 GET 导出（会写入成功冷却）。
	exported := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if exported.Code != http.StatusOK {
		t.Fatalf("GET 导出 status=%d body=%s", exported.Code, exported.Body.String())
	}
	if entries := imgExportTestZip(t, exported.Body.Bytes()); len(entries) != 3 {
		t.Fatalf("归档条目数 = %d，期望 README+manifest+1 张图片", len(entries))
	}
}

// ---- 评审 P2-2：batch 的回收站动作只接受登录 Cookie ----
//
// 单张 /restore 与 ?permanent=true 早就只认 Cookie（回收站是账号私有视图，物理删除不可逆）；
// batch 之前用 authenticate(w, r, true)，同一把 API Key 能把整个回收站一次删掉。这里断言
// restore/purge 与单张一致返回 401 且没有任何副作用，同时其它 action 仍然接受 Bearer。

func TestImagesBatchTrashActionsRequireCookie(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-cookie-only@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	apiKey := env.addAPIKey(t, 1)

	active, _ := imgTrashTestUpload(t, env, auth, "cookie-only-active.png", nil)
	trashed := ""
	for _, action := range []string{"restore", "purge"} {
		t.Run("API Key 拒绝 "+action, func(t *testing.T) {
			// 每个 action 用独立的回收站记录：失败信息才能反映"这一批到底做了什么"。
			imageID, _ := imgTrashTestUpload(t, env, auth, "cookie-only-"+action+".png", []string{"keep"})
			imgTestTrash(t, env, auth, imageID)
			if action == "restore" {
				trashed = imageID
			}
			originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
			beforeDeleted, beforeVisible, beforeUpdated, beforeTags := imgExtraTestRow(t, env, imageID)
			recorder := imgExtraTestBatch(t, env, imgTestBearerAuth(apiKey), map[string]any{
				"ids": []string{imageID}, "action": action,
			})
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("API Key 的 batch %s status=%d，期望 401 body=%s", action, recorder.Code, recorder.Body.String())
			}
			if code := imgTestErrorCode(t, recorder); code != "UNAUTHORIZED" {
				t.Fatalf("error.code = %q，期望 UNAUTHORIZED", code)
			}
			afterDeleted, afterVisible, afterUpdated, afterTags := imgExtraTestRow(t, env, imageID)
			if (afterDeleted == nil) != (beforeDeleted == nil) || afterVisible != beforeVisible ||
				afterUpdated != beforeUpdated || strings.Join(afterTags, ",") != strings.Join(beforeTags, ",") {
				t.Fatalf("API Key 的 batch %s 产生了副作用: deleted=%v visible=%d updated=%q tags=%v",
					action, afterDeleted, afterVisible, afterUpdated, afterTags)
			}
			if imgTrashTestFileMissing(t, originalAbs) || imgTrashTestFileMissing(t, thumbAbs) {
				t.Fatalf("API Key 的 batch %s 动了磁盘字节", action)
			}
		})
	}

	// 其它 action 保持原契约：Bearer 仍可改可见性 / 改标签。
	for _, testCase := range []struct {
		name string
		body map[string]any
	}{
		{name: "visibility", body: map[string]any{"ids": []string{active}, "action": "visibility", "visible": false}},
		{name: "tag", body: map[string]any{"ids": []string{active}, "action": "tag", "tags": []string{"via-key"}}},
	} {
		recorder := imgExtraTestBatch(t, env, imgTestBearerAuth(apiKey), testCase.body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("Bearer + %s status=%d，期望 200 body=%s", testCase.name, recorder.Code, recorder.Body.String())
		}
	}
	if _, visible, _, tags := imgExtraTestRow(t, env, active); visible != 0 || strings.Join(tags, ",") != "via-key" {
		t.Fatalf("Bearer 的可见性/标签动作没有生效: visible=%d tags=%v", visible, tags)
	}

	// Cookie 仍可恢复，再删除后仍可彻底删除（响应含 updatedIds 供前端精确移除）。
	if recorder := imgExtraTestBatch(t, env, auth, map[string]any{"ids": []string{trashed}, "action": "restore"}); recorder.Code != http.StatusOK {
		t.Fatalf("Cookie 批量恢复 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imgTestTrash(t, env, auth, trashed)
	purged := imgExtraTestBatch(t, env, auth, map[string]any{"ids": []string{trashed}, "action": "purge"})
	if purged.Code != http.StatusOK {
		t.Fatalf("Cookie 批量彻底删除 status=%d body=%s", purged.Code, purged.Body.String())
	}
	if ids := imgExtraTestStringList(t, imgTestJSON(t, purged), "updatedIds"); len(ids) != 1 || ids[0] != trashed {
		t.Fatalf("purge 的 updatedIds = %v，期望 [%s]", ids, trashed)
	}
}

// ---- 评审 P2-4：批量恢复也必须校验原图字节 ----
//
// 单张恢复用 imagesRequireRegularFile 拒绝缺失原图（409）；batch 之前只做行数核对，
// 会把"永远取不到字节"的记录放回图库（列表可见、原图/缩略图 404、也不会被保留期清理）。

func TestImagesBatchRestoreRejectsMissingBytes(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "batch-restore-bytes@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first, _ := imgTrashTestUpload(t, env, auth, "restore-a.png", nil)
	second, _ := imgTrashTestUpload(t, env, auth, "restore-b.png", nil)
	imgTestTrash(t, env, auth, first)
	imgTestTrash(t, env, auth, second)
	firstOriginal, _ := imgTrashTestPaths(t, env, first)
	if err := os.Remove(firstOriginal); err != nil {
		t.Fatalf("删除原图失败: %v", err)
	}

	beforeFirstDeleted, _, beforeFirstUpdated, _ := imgExtraTestRow(t, env, first)
	beforeSecondDeleted, _, beforeSecondUpdated, _ := imgExtraTestRow(t, env, second)

	// 整批 409，且**没有任何一张**被恢复（不能部分成功）。
	recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "restore",
	})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("缺字节的批量恢复 status=%d，期望 409 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "IMAGE_BYTES_MISSING" {
		t.Fatalf("error.code = %q，期望 IMAGE_BYTES_MISSING", code)
	}
	afterFirstDeleted, _, afterFirstUpdated, _ := imgExtraTestRow(t, env, first)
	afterSecondDeleted, _, afterSecondUpdated, _ := imgExtraTestRow(t, env, second)
	if afterFirstDeleted == nil || afterSecondDeleted == nil {
		t.Fatalf("409 之后仍有图片被恢复: first=%v second=%v", afterFirstDeleted, afterSecondDeleted)
	}
	if beforeFirstDeleted == nil || beforeSecondDeleted == nil {
		t.Fatalf("前置条件错误：图片应在回收站中")
	}
	if afterFirstUpdated != beforeFirstUpdated || afterSecondUpdated != beforeSecondUpdated {
		t.Fatalf("409 之后 updatedAt 被改写: %q->%q %q->%q",
			beforeFirstUpdated, afterFirstUpdated, beforeSecondUpdated, afterSecondUpdated)
	}

	// 与单张恢复的错误码一致。
	if single := imgTrashTestRestore(t, env, auth, first); single.Code != http.StatusConflict {
		t.Fatalf("单张恢复 status=%d，期望 409 body=%s", single.Code, single.Body.String())
	}

	// 修好字节后同一批次可以恢复。
	if err := os.WriteFile(firstOriginal, imgTestPNG(t, 24, 18), 0o640); err != nil {
		t.Fatalf("恢复原图字节失败: %v", err)
	}
	if recorder := imgExtraTestBatch(t, env, auth, map[string]any{
		"ids": []string{first, second}, "action": "restore",
	}); recorder.Code != http.StatusOK {
		t.Fatalf("修好字节后批量恢复 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, id := range []string{first, second} {
		if deletedAt, _, _, _ := imgExtraTestRow(t, env, id); deletedAt != nil {
			t.Fatalf("图片 %s 未被恢复", id)
		}
	}
}

// ---- 上传频率限流接线：from-url 必须在下载与占槽之前被同一桶拦截 ----
//
// core 在 images_upload.go 提供 limitImageUpload（100 次/小时/可信 clientIP，与 multipart
// 共用一个桶）。这里断言 from-url 确实调用了它：被限流时不下载、不占用上传槽。
func TestImagesFromURLRespectsUploadRateLimit(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "from-url-ratelimit@example.com", "password123")

	// httptest.NewRequest 的 RemoteAddr 固定为 192.0.2.1:1234；用它把该 IP 的桶填满。
	probe := httptest.NewRequest(http.MethodPost, "/api/images/from-url", nil)
	for attempt := 0; attempt < uploadImageRateLimit; attempt++ {
		if !env.app.limitImageUpload(httptest.NewRecorder(), probe) {
			t.Fatalf("桶容量 %d，第 %d 次就被限流", uploadImageRateLimit, attempt+1)
		}
	}

	payload, err := json.Marshal(map[string]any{"url": "https://example.com/rate-limited.png"})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/images/from-url", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	env.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("超出上传频率 status=%d，期望 429 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "RATE_LIMITED" {
		t.Fatalf("error.code = %q，期望 RATE_LIMITED", code)
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatalf("限流响应缺少 Retry-After")
	}
	// 被拒的请求不能留下远端下载的临时文件，也不能占着上传槽。
	entries, err := os.ReadDir(filepath.Join(env.app.cfg.DataDir, imagesTempDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读取暂存目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "remote-") {
			t.Fatalf("被限流的 from-url 留下了下载临时文件: %s", entry.Name())
		}
	}
	select {
	case env.app.uploadSlots <- struct{}{}:
		<-env.app.uploadSlots
	default:
		t.Fatalf("被限流的 from-url 占用了上传槽")
	}
}
