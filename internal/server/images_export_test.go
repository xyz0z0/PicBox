package server

// 导出接口（GET|HEAD /api/images/export）的 HTTP 级回归：范围校验、预检、
// 冷却与预约、流式 ZIP、manifest 内容、失败/取消后立即重试。

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- helpers ----

func imgExportTestDo(t *testing.T, env *imgTestEnv, method, target string, auth imgTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(t, method, target, nil, "", auth)
}

// imgExportTestWriteHandler 是一个可以"中途失败"的 ResponseWriter，用来确定性复现
// 客户端断开/写入失败：成功写出响应头之后，后续 Write 返回错误。
type imgExportTestWriteHandler struct {
	header    http.Header
	status    int
	written   int
	failAfter int
	onHeader  func()
}

func (w *imgExportTestWriteHandler) Header() http.Header { return w.header }

func (w *imgExportTestWriteHandler) WriteHeader(status int) {
	w.status = status
	if w.onHeader != nil {
		w.onHeader()
	}
}

func (w *imgExportTestWriteHandler) Write(body []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, errors.New("client gone")
	}
	w.written += len(body)
	return len(body), nil
}

func (w *imgExportTestWriteHandler) Flush() {}

func imgExportTestZip(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("响应不是合法 ZIP: %v", err)
	}
	entries := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			t.Fatalf("打开归档条目 %s 失败: %v", file.Name, err)
		}
		content, err := io.ReadAll(opened)
		opened.Close()
		if err != nil {
			t.Fatalf("读取归档条目 %s 失败: %v", file.Name, err)
		}
		entries[file.Name] = content
	}
	return entries
}

func imgExportTestState(t *testing.T, env *imgTestEnv, userID int64) (lastExportAt, reservationAt, token sql.NullString) {
	t.Helper()
	if err := env.app.db.QueryRow(
		`SELECT lastExportAt, exportReservationAt, exportReservationToken FROM "User" WHERE id = ?`, userID).
		Scan(&lastExportAt, &reservationAt, &token); err != nil {
		t.Fatalf("读取导出状态失败: %v", err)
	}
	return lastExportAt, reservationAt, token
}

func imgExportTestSetState(t *testing.T, env *imgTestEnv, userID int64, lastExportAt, reservationAt, token any) {
	t.Helper()
	if _, err := env.app.db.Exec(
		`UPDATE "User" SET lastExportAt = ?, exportReservationAt = ?, exportReservationToken = ? WHERE id = ?`,
		lastExportAt, reservationAt, token, userID); err != nil {
		t.Fatalf("设置导出状态失败: %v", err)
	}
}

// ---- 成功导出：headers + manifest + 归档字节 ----

func TestImagesExportStreamsArchiveWithManifest(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(12))
	cookie := env.registerAdmin(t, "export-owner@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	visibleID, visibleData := imgTrashTestUpload(t, env, auth, "visible.png", []string{"zulu", "alpha"})
	hiddenID, hiddenData := imgTrashTestUpload(t, env, auth, "hidden.png", nil)
	imgTestSetHidden(t, env, auth, hiddenID, false)
	trashedID, _ := imgTrashTestUpload(t, env, auth, "trashed.png", nil)
	imgTestTrash(t, env, auth, trashedID)

	head := imgExportTestDo(t, env, http.MethodHead, "/api/images/export?scope=all", auth)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD 预检 status=%d body=%s", head.Code, head.Body.String())
	}
	if count := head.Header().Get("X-Export-Count"); count != "2" {
		t.Fatalf("HEAD X-Export-Count = %q，期望 2（隐藏图片也导出，回收站不导出）", count)
	}
	if bytes := head.Header().Get("X-Export-Bytes"); bytes != "" {
		var total int64
		if err := env.app.db.QueryRow(
			`SELECT SUM(size) FROM "Image" WHERE deletedAt IS NULL AND deletedByAdmin = 0`).Scan(&total); err != nil {
			t.Fatalf("统计字节失败: %v", err)
		}
		if want := int64(len(visibleData) + len(hiddenData)); total != want {
			t.Fatalf("数据库字节统计异常: %d vs %d", total, want)
		}
	}
	if hours := head.Header().Get("X-Export-Cooldown-Hours"); hours != "12" {
		t.Fatalf("HEAD X-Export-Cooldown-Hours = %q", hours)
	}
	if last, reservation, _ := imgExportTestState(t, env, 1); last.Valid || reservation.Valid {
		t.Fatalf("HEAD 不得声明预约或消耗冷却: last=%v reservation=%v", last, reservation)
	}

	recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET 导出 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "private, no-store" {
		t.Fatalf("Cache-Control = %q", cache)
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, `attachment; filename="picbox-images-all-`) || !strings.HasSuffix(disposition, `.zip"`) {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	if bytesHeader := recorder.Header().Get("X-Export-Bytes"); bytesHeader == "" {
		t.Fatalf("缺少 X-Export-Bytes")
	}

	entries := imgExportTestZip(t, recorder.Body.Bytes())
	readme, ok := entries[imagesExportReadmeName]
	if !ok || !strings.Contains(string(readme), "全部图片 (all)") {
		t.Fatalf("README 内容异常: %q", string(readme))
	}
	manifestBody, ok := entries[imagesExportManifestName]
	if !ok {
		t.Fatalf("归档缺少 manifest.json，条目=%v", imgExportTestEntryNames(entries))
	}
	var manifest imagesExportManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v", err)
	}
	if manifest.Version != imagesExportVersion || manifest.Scope != "all" || manifest.Format != "zip" || manifest.Count != 2 {
		t.Fatalf("manifest 头字段异常: %+v", manifest)
	}
	if manifest.GeneratedAt == "" {
		t.Fatalf("manifest 缺少 generatedAt")
	}
	countHeader := recorder.Header().Get("X-Export-Count")
	bytesHeader := recorder.Header().Get("X-Export-Bytes")
	if countHeader != "2" {
		t.Fatalf("X-Export-Count = %q", countHeader)
	}
	// 清单与响应头必须一致。
	if manifest.Count != 2 || bytesHeader != strconv.FormatInt(manifest.TotalBytes, 10) {
		t.Fatalf("manifest 与响应头不一致: count=%d total=%d header=%q", manifest.Count, manifest.TotalBytes, bytesHeader)
	}
	if manifest.TotalBytes != int64(len(visibleData)+len(hiddenData)) {
		t.Fatalf("manifest.TotalBytes = %d，期望 %d", manifest.TotalBytes, len(visibleData)+len(hiddenData))
	}

	byID := map[string]imagesExportManifestEntry{}
	for _, entry := range manifest.Images {
		byID[entry.ID] = entry
	}
	for _, imageID := range []string{visibleID, hiddenID} {
		entry, ok := byID[imageID]
		if !ok {
			t.Fatalf("manifest 缺少图片 %s", imageID)
		}
		if entry.ArchivePath != "images/"+imageID+".png" || entry.ContentType != "image/png" {
			t.Fatalf("manifest 归档目标异常: %+v", entry)
		}
		if entry.Width != 24 || entry.Height != 18 || entry.CreatedAt == "" || entry.UpdatedAt == "" {
			t.Fatalf("manifest 元数据异常: %+v", entry)
		}
		if entry.OriginalMimeType != "image/png" {
			t.Fatalf("manifest originalMimeType = %q", entry.OriginalMimeType)
		}
		content, ok := entries[entry.ArchivePath]
		if !ok {
			t.Fatalf("归档缺少 %s，条目=%v", entry.ArchivePath, imgExportTestEntryNames(entries))
		}
		if !bytes.Equal(content, visibleData) && !bytes.Equal(content, hiddenData) {
			t.Fatalf("归档字节与上传字节不一致: %s", entry.ArchivePath)
		}
		if entry.Size != int64(len(content)) {
			t.Fatalf("manifest size = %d，归档实际 %d", entry.Size, len(content))
		}
	}
	if visible := byID[visibleID]; strings.Join(visible.Tags, ",") != "alpha,zulu" {
		t.Fatalf("manifest tags = %v，期望排序后的 alpha,zulu", visible.Tags)
	}
	if hidden := byID[hiddenID]; hidden.IsVisible {
		t.Fatalf("manifest isVisible 应为 false")
	}
	if _, ok := byID[trashedID]; ok {
		t.Fatalf("回收站图片不应出现在导出清单中")
	}

	// 成功后进入冷却；HEAD 仍可预检，但 GET 已受限。
	last, reservation, token := imgExportTestState(t, env, 1)
	if !last.Valid || reservation.Valid || token.Valid {
		t.Fatalf("成功导出后状态异常: last=%v reservation=%v token=%v", last, reservation, token)
	}
	limited := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("冷却期内 GET status=%d，期望 429 body=%s", limited.Code, limited.Body.String())
	}
	if code := imgTestErrorCode(t, limited); code != "EXPORT_COOLDOWN" {
		t.Fatalf("error.code = %q", code)
	}
	if retryAfter := limited.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatalf("429 缺少 Retry-After")
	}
	if contentType := limited.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("GET 错误 Content-Type = %q", contentType)
	}
	if cache := limited.Header().Get("Cache-Control"); cache != "private, no-store" {
		t.Fatalf("GET 错误 Cache-Control = %q", cache)
	}
	headLimited := imgExportTestDo(t, env, http.MethodHead, "/api/images/export?scope=all", auth)
	if headLimited.Code != http.StatusTooManyRequests {
		t.Fatalf("冷却期内 HEAD status=%d，期望 429", headLimited.Code)
	}
	if code := headLimited.Header().Get("X-Export-Error"); code != "EXPORT_COOLDOWN" {
		t.Fatalf("HEAD X-Export-Error = %q", code)
	}
	if message := headLimited.Header().Get("X-Export-Message"); message == "" || strings.Contains(message, " ") {
		t.Fatalf("HEAD X-Export-Message 必须经过 URL 编码: %q", message)
	}
	if len(headLimited.Body.Bytes()) != 0 {
		t.Fatalf("HEAD 错误响应不应带 body")
	}
}

func imgExportTestEntryNames(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	return names
}

// ---- 范围与选择 ----

func TestImagesExportScopeValidation(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(0))
	cookie := env.registerAdmin(t, "export-scope@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	// 空库：合法范围但没有图片。
	empty := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if empty.Code != http.StatusNotFound {
		t.Fatalf("空库导出 status=%d，期望 404 body=%s", empty.Code, empty.Body.String())
	}
	if code := imgTestErrorCode(t, empty); code != "NO_IMAGES" {
		t.Fatalf("error.code = %q", code)
	}

	imageID, imageData := imgTrashTestUpload(t, env, auth, "scope.png", nil)
	_, otherCookie := env.addUser(t, "export-other@example.com")
	foreignID, _ := imgTrashTestUpload(t, env, imgTestCookieAuth(otherCookie), "foreign.png", nil)

	manyIDs := make([]string, 0, imagesExportMaxSelected+1)
	for index := 0; index <= imagesExportMaxSelected; index++ {
		manyIDs = append(manyIDs, uuid.NewString())
	}

	cases := []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{name: "no-scope", query: "", status: 400, code: "INVALID_SCOPE"},
		{name: "bad-scope", query: "?scope=some", status: 400, code: "INVALID_SCOPE"},
		{name: "all-with-ids", query: "?scope=all&ids=" + imageID, status: 400, code: "INVALID_SCOPE"},
		{name: "ids-without-ids", query: "?scope=ids", status: 400, code: "INVALID_IDS"},
		{name: "bad-uuid", query: "?ids=not-a-uuid", status: 400, code: "INVALID_IDS"},
		{name: "too-many", query: "?" + imgExportTestIDQuery(manyIDs), status: 400, code: "TOO_MANY_SELECTED"},
		{name: "foreign-id", query: "?ids=" + foreignID, status: 403, code: "FORBIDDEN"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export"+testCase.query, auth)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d，期望 %d body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
			if code := imgTestErrorCode(t, recorder); code != testCase.code {
				t.Fatalf("error.code = %q，期望 %q", code, testCase.code)
			}
			// 校验失败不得消耗冷却，也不得留下预约。
			if last, reservation, _ := imgExportTestState(t, env, 1); last.Valid || reservation.Valid {
				t.Fatalf("校验失败后状态被改写: last=%v reservation=%v", last, reservation)
			}
		})
	}

	// 显式选择：顺序与请求一致，且只包含指定图片。
	selected := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=ids&ids="+imageID, auth)
	if selected.Code != http.StatusOK {
		t.Fatalf("选择导出 status=%d body=%s", selected.Code, selected.Body.String())
	}
	entries := imgExportTestZip(t, selected.Body.Bytes())
	content, ok := entries["images/"+imageID+".png"]
	if !ok || !bytes.Equal(content, imageData) {
		t.Fatalf("选择导出的归档内容异常")
	}
	if count := selected.Header().Get("X-Export-Count"); count != "1" {
		t.Fatalf("X-Export-Count = %q", count)
	}
	disposition := selected.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "picbox-images-selected-") {
		t.Fatalf("选择导出的文件名后缀异常: %q", disposition)
	}

	// 未认证。
	unauthorized := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", imgTestAuth{})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("未登录导出 status=%d", unauthorized.Code)
	}
}

func imgExportTestIDQuery(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "ids="+id)
	}
	return strings.Join(parts, "&")
}

// ---- 预检：缺失文件与总量上限 ----

func TestImagesExportPreflightMissingFileAndSizeLimit(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(0))
	cookie := env.registerAdmin(t, "export-preflight@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	imageID, _ := imgTrashTestUpload(t, env, auth, "missing.png", nil)
	originalAbs, _ := imgTrashTestPaths(t, env, imageID)
	if err := os.Remove(originalAbs); err != nil {
		t.Fatalf("删除原图失败: %v", err)
	}
	missing := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("缺失文件 GET status=%d，期望 404 body=%s", missing.Code, missing.Body.String())
	}
	if code := imgTestErrorCode(t, missing); code != "EXPORT_FILE_MISSING" {
		t.Fatalf("error.code = %q", code)
	}
	missingHead := imgExportTestDo(t, env, http.MethodHead, "/api/images/export?scope=all", auth)
	if missingHead.Code != http.StatusNotFound || missingHead.Header().Get("X-Export-Error") != "EXPORT_FILE_MISSING" {
		t.Fatalf("缺失文件 HEAD status=%d code=%q", missingHead.Code, missingHead.Header().Get("X-Export-Error"))
	}
	if last, reservation, _ := imgExportTestState(t, env, 1); last.Valid || reservation.Valid {
		t.Fatalf("预检失败后状态被改写")
	}

	// 用稀疏文件构造 2 GiB 边界，只做 HEAD 预检，不实际读取字节。
	if err := os.WriteFile(originalAbs, []byte("sparse"), 0o640); err != nil {
		t.Fatalf("重建原图失败: %v", err)
	}
	if err := os.Truncate(originalAbs, 2*1024*1024*1024); err != nil {
		t.Fatalf("构造稀疏文件失败: %v", err)
	}
	atLimit := imgExportTestDo(t, env, http.MethodHead, "/api/images/export?scope=all", auth)
	if atLimit.Code != http.StatusOK {
		t.Fatalf("恰好 2 GiB 应允许: status=%d body=%s", atLimit.Code, atLimit.Body.String())
	}
	if got := atLimit.Header().Get("X-Export-Bytes"); got != "2147483648" {
		t.Fatalf("X-Export-Bytes = %q，期望 2147483648", got)
	}
	if err := os.Truncate(originalAbs, 2*1024*1024*1024+1); err != nil {
		t.Fatalf("构造超限稀疏文件失败: %v", err)
	}
	tooLarge := imgExportTestDo(t, env, http.MethodHead, "/api/images/export?scope=all", auth)
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超 2 GiB status=%d，期望 413", tooLarge.Code)
	}
	if code := tooLarge.Header().Get("X-Export-Error"); code != "EXPORT_TOO_LARGE" {
		t.Fatalf("HEAD X-Export-Error = %q", code)
	}
}

// ---- 并发：只有一个赢家 ----

func TestImagesExportConcurrentOnlyOneWinner(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(12))
	cookie := env.registerAdmin(t, "export-race@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	for index := 0; index < 4; index++ {
		imgTrashTestUpload(t, env, auth, "race-"+uuid.NewString()+".png", nil)
	}

	type result struct {
		status int
		code   string
	}
	results := make([]result, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
			code := ""
			if recorder.Code != http.StatusOK {
				body := map[string]any{}
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err == nil {
					if errorBody, ok := body["error"].(map[string]any); ok {
						code, _ = errorBody["code"].(string)
					}
				}
			}
			results[slot] = result{status: recorder.Code, code: code}
		}(index)
	}
	wait.Wait()

	successes := 0
	for _, entry := range results {
		if entry.status == http.StatusOK {
			successes++
			continue
		}
		if entry.status != http.StatusTooManyRequests {
			t.Fatalf("并发导出出现意外状态: %+v", entry)
		}
		if entry.code != "EXPORT_IN_PROGRESS" && entry.code != "EXPORT_COOLDOWN" {
			t.Fatalf("并发失败码 = %q", entry.code)
		}
	}
	if successes != 1 {
		t.Fatalf("并发导出赢家数量 = %d，期望恰好 1（results=%+v）", successes, results)
	}
	last, reservation, token := imgExportTestState(t, env, 1)
	if !last.Valid || reservation.Valid || token.Valid {
		t.Fatalf("并发收尾后状态异常: last=%v reservation=%v token=%v", last, reservation, token)
	}
}

// ---- 租约、续租与 token 条件化 ----

func TestImagesExportLeaseTakeoverAndTokenConditions(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(0))
	cookie := env.registerAdmin(t, "export-lease@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imgTrashTestUpload(t, env, auth, "lease.png", nil)

	previousLease, previousRenew := imagesExportLease, imagesExportRenewInterval
	imagesExportLease = time.Minute
	imagesExportRenewInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		imagesExportLease = previousLease
		imagesExportRenewInterval = previousRenew
	})

	// 未过期的预约：GET 受限，HEAD 也受限（HEAD 只读状态，不 claim）。
	staleAt := time.Now().UTC().Add(-30 * time.Second).Format(imagesTimeLayout)
	imgExportTestSetState(t, env, 1, nil, staleAt, "holder-token")
	limited := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("未过期预约 status=%d，期望 429", limited.Code)
	}
	if code := imgTestErrorCode(t, limited); code != "EXPORT_IN_PROGRESS" {
		t.Fatalf("error.code = %q，期望 EXPORT_IN_PROGRESS", code)
	}

	// 过期租约可以被接管。
	expiredAt := time.Now().UTC().Add(-2 * imagesExportLease).Format(imagesTimeLayout)
	imgExportTestSetState(t, env, 1, nil, expiredAt, "holder-token")
	takeover := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if takeover.Code != http.StatusOK {
		t.Fatalf("租约过期后 status=%d，期望 200 body=%s", takeover.Code, takeover.Body.String())
	}

	// token 条件化：错误 token 不能释放/收尾/续租，也不影响当前持有者。
	imgExportTestSetState(t, env, 1, nil, imagesNow(), "current-token")
	ctx := context.Background()
	if ok, err := env.app.imagesExportRelease(ctx, 1, "other-token"); err != nil || ok {
		t.Fatalf("错误 token 释放应无效果: ok=%v err=%v", ok, err)
	}
	if ok, err := env.app.imagesExportFinalize(ctx, 1, "other-token", time.Now()); err != nil || ok {
		t.Fatalf("错误 token 收尾应无效果: ok=%v err=%v", ok, err)
	}
	if ok, err := env.app.imagesExportRenew(ctx, 1, "other-token"); err != nil || ok {
		t.Fatalf("错误 token 续租应无效果: ok=%v err=%v", ok, err)
	}
	last, reservation, token := imgExportTestState(t, env, 1)
	if last.Valid || !reservation.Valid || token.String != "current-token" {
		t.Fatalf("错误 token 影响了当前预约: last=%v reservation=%v token=%v", last, reservation, token)
	}

	// 续租：按 token 更新 exportReservationAt。
	before := reservation.String
	time.Sleep(5 * time.Millisecond)
	if ok, err := env.app.imagesExportRenew(ctx, 1, "current-token"); err != nil || !ok {
		t.Fatalf("续租失败: ok=%v err=%v", ok, err)
	}
	_, renewed, renewedToken := imgExportTestState(t, env, 1)
	if renewed.String == before || renewedToken.String != "current-token" {
		t.Fatalf("续租未更新时间或改动了 token: %v -> %v (%v)", before, renewed.String, renewedToken.String)
	}

	// 租约丢失：后台续租协程必须回调 onLost 并自行退出。
	lost := make(chan struct{}, 1)
	stop := env.app.imagesExportKeepLease(context.Background(), 1, "ghost-token", func() {
		select {
		case lost <- struct{}{}:
		default:
		}
	})
	defer stop()
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatalf("租约丢失后没有触发 onLost")
	}
}

// ---- 成功才冷却；失败/取消立即重试 ----

func TestImagesExportFailureAndCancelAllowImmediateRetry(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(12))
	cookie := env.registerAdmin(t, "export-retry@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imageID, _ := imgTrashTestUpload(t, env, auth, "retry.png", nil)
	if imageID == "" {
		t.Fatalf("上传失败")
	}

	// 写入失败：响应头已发出后客户端断开。必须立刻释放预约，且不写成功冷却。
	request := httptest.NewRequest(http.MethodGet, "/api/images/export?scope=all", nil)
	request.AddCookie(cookie)
	failing := &imgExportTestWriteHandler{header: http.Header{}, failAfter: 0}
	env.handler.ServeHTTP(failing, request)
	if failing.status != http.StatusOK {
		t.Fatalf("写入失败场景的响应头 status=%d", failing.status)
	}
	if last, reservation, _ := imgExportTestState(t, env, 1); last.Valid || reservation.Valid {
		t.Fatalf("失败导出留下了冷却或预约: last=%v reservation=%v", last, reservation)
	}
	retry := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if retry.Code != http.StatusOK {
		t.Fatalf("失败后立即重试 status=%d，期望 200 body=%s", retry.Code, retry.Body.String())
	}

	// 取消：请求 context 在流式写入期间取消。先清掉上一步写入的冷却。
	imgExportTestSetState(t, env, 1, nil, nil, nil)
	cancelCtx, cancel := context.WithCancel(context.Background())
	canceledRequest := httptest.NewRequest(http.MethodGet, "/api/images/export?scope=all", nil).WithContext(cancelCtx)
	canceledRequest.AddCookie(cookie)
	canceled := &imgExportTestWriteHandler{header: http.Header{}, failAfter: 0, onHeader: cancel}
	env.handler.ServeHTTP(canceled, canceledRequest)
	cancel()
	if last, reservation, _ := imgExportTestState(t, env, 1); last.Valid || reservation.Valid {
		t.Fatalf("取消的导出留下了冷却或预约: last=%v reservation=%v", last, reservation)
	}
	again := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if again.Code != http.StatusOK {
		t.Fatalf("取消后立即重试 status=%d，期望 200 body=%s", again.Code, again.Body.String())
	}

	// 成功之后才进入冷却。
	if last, _, _ := imgExportTestState(t, env, 1); !last.Valid {
		t.Fatalf("成功导出后没有写入冷却")
	}
	blocked := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("成功后应立即进入冷却: status=%d", blocked.Code)
	}

	// 关闭冷却后可以连续导出（lastExportAt 仍会被记录，但不再参与限流）。
	env.app.cfg.ExportCooldownHours = 0
	for attempt := 0; attempt < 2; attempt++ {
		if recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth); recorder.Code != http.StatusOK {
			t.Fatalf("关闭冷却后第 %d 次导出 status=%d body=%s", attempt+1, recorder.Code, recorder.Body.String())
		}
	}
}

// ---- 源文件字节数与预检不一致：失败而不是产出与 manifest 不符的归档 ----

func TestImagesExportAddFileRejectsByteCountMismatch(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(0))
	cookie := env.registerAdmin(t, "export-shrink@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imageID, data := imgTrashTestUpload(t, env, auth, "shrink.png", nil)
	originalAbs, _ := imgTrashTestPaths(t, env, imageID)

	var relPath string
	if err := env.app.db.QueryRow(`SELECT originalPath FROM "Image" WHERE id = ?`, imageID).Scan(&relPath); err != nil {
		t.Fatalf("读取存储路径失败: %v", err)
	}
	image := imagesExportImage{ID: imageID, OriginalPath: relPath, OriginalMimeType: "image/png"}

	// 预检拿到 100 字节，但文件只有 50 字节：必须报错，绝不写一个与 manifest 不符的条目。
	if err := os.WriteFile(originalAbs, data[:len(data)/2], 0o640); err != nil {
		t.Fatalf("缩短原图失败: %v", err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	err := env.app.imagesExportAddFile(context.Background(), archive, image, int64(len(data)))
	archive.Close()
	if err == nil {
		t.Fatalf("字节数不符时必须返回错误")
	}
	if !strings.Contains(err.Error(), "导出过程中发生变化") {
		t.Fatalf("错误信息未说明字节变化: %v", err)
	}

	// 恢复字节后可以正常写入归档。
	if err := os.WriteFile(originalAbs, data, 0o640); err != nil {
		t.Fatalf("恢复原图失败: %v", err)
	}
	buffer.Reset()
	archive = zip.NewWriter(&buffer)
	if err := env.app.imagesExportAddFile(context.Background(), archive, image, int64(len(data))); err != nil {
		t.Fatalf("正常写入失败: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("关闭归档失败: %v", err)
	}
	entries := imgExportTestZip(t, buffer.Bytes())
	content, ok := entries["images/"+imageID+".png"]
	if !ok || !bytes.Equal(content, data) {
		t.Fatalf("归档条目内容异常")
	}

	// 端到端重试：字节一致时导出成功。
	if retry := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth); retry.Code != http.StatusOK {
		t.Fatalf("恢复后导出 status=%d body=%s", retry.Code, retry.Body.String())
	}
}

// ---- 归档路径只来自服务端生成的数据 ----

func TestImagesExportRejectsUnknownStoredExtension(t *testing.T) {
	env := imgTestNewApp(t, Config{}.WithExportCooldown(0))
	cookie := env.registerAdmin(t, "export-ext@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imageID, _ := imgTrashTestUpload(t, env, auth, "ext.png", nil)

	if _, err := env.app.db.Exec(`UPDATE "Image" SET originalPath = ? WHERE id = ?`, "images/x/evil.txt", imageID); err != nil {
		t.Fatalf("改写存储路径失败: %v", err)
	}
	recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("未知存储扩展名 status=%d，期望 500 body=%s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "EXPORT_FAILED" {
		t.Fatalf("error.code = %q", code)
	}
	// 归档内绝不能出现主机目录或原始文件名。
	if strings.Contains(recorder.Body.String(), env.app.cfg.DataDir) || strings.Contains(recorder.Body.String(), "evil.txt") {
		t.Fatalf("错误响应泄露了主机路径")
	}
}

// ---- 时间戳必须与宿主时区无关（评审 P1-1）----
//
// imagesTimeLayout 的 Z 是字面字符，不做时区换算：把本地时区的 time.Now() 直接格式化，
// 会让预约/冷却整体偏移 δ（δ>0 冷却变成 12h+δ、遗留租约变成 10min+δ；δ<0 冷却被缩短、
// 租约一写入就已过期，单飞失效）。下面的用例把宿主时区固定成东八区/西五区，断言写入值
// 解析后 ≈ 当前时刻，并覆盖租约 10 分钟、成功冷却 12 小时与并发单飞。

// imgExportTestSetLocalTZ 固定宿主时区。必须在创建 App 之前调用：这样 App.Close 的
// t.Cleanup 会先执行，恢复 time.Local 时已经没有后台 goroutine 在读这个全局变量。
func imgExportTestSetLocalTZ(t *testing.T, name string, offsetSeconds int) {
	t.Helper()
	previous := time.Local
	time.Local = time.FixedZone(name, offsetSeconds)
	t.Cleanup(func() { time.Local = previous })
}

// imgExportTestAssertNearNow 断言存库时间戳解析后与当前时刻的偏差在容差内。
func imgExportTestAssertNearNow(t *testing.T, label, raw string) {
	t.Helper()
	parsed, ok := imagesParseTime(raw)
	if !ok {
		t.Fatalf("%s 无法解析: %q", label, raw)
	}
	if len(raw) != len("2006-01-02T15:04:05.000Z") || !strings.HasSuffix(raw, "Z") {
		t.Fatalf("%s 时间戳格式异常: %q", label, raw)
	}
	skew := time.Since(parsed)
	if skew < -2*time.Second || skew > 2*time.Second {
		t.Fatalf("%s 与当前时刻相差 %s（宿主时区被写进了字面 Z 时间戳）: %q", label, skew, raw)
	}
}

func imgExportTestRetryAfter(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()
	raw := recorder.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q 不是整数", raw)
	}
	return seconds
}

func TestImagesExportTimestampsIgnoreHostTimeZone(t *testing.T) {
	zones := []struct {
		name   string
		offset int
	}{
		{name: "UTC+08", offset: 8 * 60 * 60},
		{name: "UTC-05", offset: -5 * 60 * 60},
	}
	for _, zone := range zones {
		t.Run(zone.name, func(t *testing.T) {
			imgExportTestSetLocalTZ(t, zone.name, zone.offset)
			env := imgTestNewApp(t, Config{}.WithExportCooldown(12))
			cookie := env.registerAdmin(t, "tz-"+strings.NewReplacer("+", "p", "-", "m").Replace(zone.name)+"@example.com", "password123")
			auth := imgTestCookieAuth(cookie)
			imgTrashTestUpload(t, env, auth, "tz.png", nil)

			ctx := context.Background()
			state, err := env.app.imagesExportReadState(ctx, 1)
			if err != nil {
				t.Fatalf("读取导出状态失败: %v", err)
			}
			if state.reservationAt.Valid || state.lastExportAt.Valid {
				t.Fatalf("初始状态应为空: %+v", state)
			}

			// 1) claim 写入的预约必须 ≈ 当前时刻。
			token, acquired, err := env.app.imagesExportClaim(ctx, 1, state, time.Now())
			if err != nil || !acquired {
				t.Fatalf("claim 失败: acquired=%v err=%v", acquired, err)
			}
			_, reservationRaw, storedToken := imgExportTestState(t, env, 1)
			if !reservationRaw.Valid || storedToken.String != token {
				t.Fatalf("claim 后状态异常: reservation=%v token=%q", reservationRaw, storedToken.String)
			}
			imgExportTestAssertNearNow(t, "claim 的 exportReservationAt", reservationRaw.String)

			// 2) 租约 10 分钟：刚 claim 的预约必须让 GET 受限约 600 秒。
			inProgress := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
			if inProgress.Code != http.StatusTooManyRequests {
				t.Fatalf("进行中预约 status=%d，期望 429 body=%s", inProgress.Code, inProgress.Body.String())
			}
			if code := imgTestErrorCode(t, inProgress); code != "EXPORT_IN_PROGRESS" {
				t.Fatalf("error.code = %q，期望 EXPORT_IN_PROGRESS", code)
			}
			if retry := imgExportTestRetryAfter(t, inProgress); retry < 590 || retry > 600 {
				t.Fatalf("租约 Retry-After = %d，期望约 600 秒（10 分钟租约）", retry)
			}

			// 3) 成功导出后 12 小时冷却。
			if released, err := env.app.imagesExportRelease(ctx, 1, token); err != nil || !released {
				t.Fatalf("释放预约失败: released=%v err=%v", released, err)
			}
			exported := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
			if exported.Code != http.StatusOK {
				t.Fatalf("导出 status=%d body=%s", exported.Code, exported.Body.String())
			}
			lastRaw, reservationRaw, tokenRaw := imgExportTestState(t, env, 1)
			if !lastRaw.Valid || reservationRaw.Valid || tokenRaw.Valid {
				t.Fatalf("成功导出后状态异常: last=%v reservation=%v token=%v", lastRaw, reservationRaw, tokenRaw)
			}
			imgExportTestAssertNearNow(t, "finalize 的 lastExportAt", lastRaw.String)

			cooling := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
			if cooling.Code != http.StatusTooManyRequests {
				t.Fatalf("冷却期内 status=%d，期望 429", cooling.Code)
			}
			if code := imgTestErrorCode(t, cooling); code != "EXPORT_COOLDOWN" {
				t.Fatalf("error.code = %q，期望 EXPORT_COOLDOWN", code)
			}
			if retry := imgExportTestRetryAfter(t, cooling); retry < 43190 || retry > 43200 {
				t.Fatalf("冷却 Retry-After = %d，期望约 43200 秒（12 小时）", retry)
			}

			// 4) 并发单飞：负偏移下若租约被写成过去时间，第二个 GET 也能 claim 成功。
			imgExportTestSetState(t, env, 1, nil, nil, nil)
			statuses := make([]int, 2)
			codes := make([]string, 2)
			var wait sync.WaitGroup
			for index := 0; index < 2; index++ {
				wait.Add(1)
				go func(slot int) {
					defer wait.Done()
					recorder := imgExportTestDo(t, env, http.MethodGet, "/api/images/export?scope=all", auth)
					statuses[slot] = recorder.Code
					if recorder.Code != http.StatusOK {
						body := map[string]any{}
						if err := json.Unmarshal(recorder.Body.Bytes(), &body); err == nil {
							if errorBody, ok := body["error"].(map[string]any); ok {
								codes[slot], _ = errorBody["code"].(string)
							}
						}
					}
				}(index)
			}
			wait.Wait()
			successes := 0
			for index, status := range statuses {
				if status == http.StatusOK {
					successes++
					continue
				}
				if status != http.StatusTooManyRequests {
					t.Fatalf("并发导出出现意外状态: %d (%s)", status, codes[index])
				}
				if codes[index] != "EXPORT_IN_PROGRESS" && codes[index] != "EXPORT_COOLDOWN" {
					t.Fatalf("并发失败码异常: %q", codes[index])
				}
			}
			if successes != 1 {
				t.Fatalf("并发导出赢家 = %d，期望恰好 1（statuses=%v，时区 %s）", successes, statuses, zone.name)
			}
		})
	}
}
