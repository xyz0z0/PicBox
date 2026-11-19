package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"picbox/frontend"
)

// webTestRoutes are the original page routes that must serve the static React
// shell with 200. They are listed explicitly so an unknown path can never look
// like a successful navigation. /images/{id} is covered separately because its
// status depends on the database record.
var webTestRoutes = []string{
	"/",
	"/dashboard",
	"/admin",
	"/forgot-password",
	"/reset-password",
	"/verify-email",
}

// webTestApp builds the same router topology as New without opening a
// database: the web layer needs none. All workers register on the single
// ServeMux, and unknown non-API paths fall through to its default 404.
func webTestApp(t *testing.T) *App {
	t.Helper()
	app := &App{mux: http.NewServeMux()}
	app.registerAuthRoutes()
	app.registerImageRoutes()
	app.registerImagesExtraRoutes()
	app.registerWebRoutes()
	return app
}

func webTestRequest(t *testing.T, app *App, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}

func webTestEmbeddedFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := frontend.Dist.ReadFile(name)
	if err != nil {
		t.Fatalf("嵌入产物缺少 %s: %v", name, err)
	}
	return data
}

func TestWebPagesServeEmbeddedShell(t *testing.T) {
	app := webTestApp(t)
	shell := webTestEmbeddedFile(t, "dist/index.html")

	for _, route := range webTestRoutes {
		t.Run(route, func(t *testing.T) {
			response := webTestRequest(t, app, http.MethodGet, route, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("%s 应返回 200: %d %s", route, response.Code, response.Body.String())
			}
			if body := response.Body.Bytes(); string(body) != string(shell) {
				t.Fatalf("%s 返回的 HTML 与嵌入的 dist/index.html 不一致（%d vs %d 字节）", route, len(body), len(shell))
			}

			header := response.Header()
			if got := header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("%s Content-Type = %q", route, got)
			}
			if got := header.Get("Cache-Control"); got != "no-store" {
				t.Fatalf("%s 页面必须 no-store，实际 %q", route, got)
			}
			if got := header.Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("%s X-Frame-Options = %q", route, got)
			}
			if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("%s X-Content-Type-Options = %q", route, got)
			}
			if got := header.Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("%s Referrer-Policy = %q", route, got)
			}
			if etag := header.Get("ETag"); !regexp.MustCompile(`^"[0-9a-f]{32}"$`).MatchString(etag) {
				t.Fatalf("%s ETag 格式不符: %q", route, etag)
			}
			if header.Get("Content-Security-Policy") != webContentSecurityPolicy {
				t.Fatalf("%s 缺少统一的 CSP", route)
			}
		})
	}
}

func TestWebPageHeadMatchesGetWithoutBody(t *testing.T) {
	app := webTestApp(t)
	shell := webTestEmbeddedFile(t, "dist/index.html")

	get := webTestRequest(t, app, http.MethodGet, "/dashboard", nil)
	head := webTestRequest(t, app, http.MethodHead, "/dashboard", nil)

	if head.Code != http.StatusOK {
		t.Fatalf("HEAD /dashboard 应返回 200: %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD /dashboard 不应返回响应体: %d 字节", head.Body.Len())
	}
	if got, want := head.Header().Get("Content-Length"), get.Header().Get("Content-Length"); got != want {
		t.Fatalf("HEAD Content-Length = %q，GET = %q", got, want)
	}
	if got, want := head.Header().Get("Content-Length"), len(shell); got != strconv.Itoa(want) {
		t.Fatalf("HEAD Content-Length = %q，嵌入文件 %d 字节", got, want)
	}
	for _, name := range []string{"Content-Type", "ETag", "Cache-Control", "Content-Security-Policy", "X-Frame-Options"} {
		if head.Header().Get(name) != get.Header().Get(name) {
			t.Fatalf("HEAD 与 GET 的 %s 不一致: %q vs %q", name, head.Header().Get(name), get.Header().Get(name))
		}
	}
}

func TestWebContentSecurityPolicyKeepsSourcesLocal(t *testing.T) {
	app := webTestApp(t)
	response := webTestRequest(t, app, http.MethodGet, "/", nil)
	policy := response.Header().Get("Content-Security-Policy")
	directives := map[string]string{}
	for _, part := range strings.Split(policy, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		directives[fields[0]] = strings.Join(fields[1:], " ")
	}

	cases := map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self'",
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data: blob:",
		"connect-src":     "'self'",
		"form-action":     "'self'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
		"object-src":      "'none'",
	}
	for directive, expected := range cases {
		if got, ok := directives[directive]; !ok || got != expected {
			t.Fatalf("CSP %s = %q，期望 %q（完整策略: %s）", directive, got, expected, policy)
		}
	}
	if strings.Contains(policy, "https:") || strings.Contains(policy, "unsafe-eval") {
		t.Fatalf("CSP 不应放宽外部来源或允许 eval: %s", policy)
	}
	if strings.Contains(directives["script-src"], "unsafe-inline") {
		t.Fatalf("script-src 不得包含 unsafe-inline: %q", directives["script-src"])
	}
}

func TestWebAssetsServeEmbeddedBundle(t *testing.T) {
	app := webTestApp(t)

	entries, err := frontend.Dist.ReadDir("dist/assets")
	if err != nil {
		t.Fatalf("嵌入产物缺少 dist/assets: %v", err)
	}

	served := 0
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("assets/ 下不允许子目录: %s", entry.Name())
		}
		name := entry.Name()
		contentType, ok := webAssetContentTypes[strings.ToLower(path.Ext(name))]
		if !ok {
			t.Fatalf("嵌入产物包含 Go 不会服务的扩展名: %s", name)
		}
		want := webTestEmbeddedFile(t, "dist/assets/"+name)

		t.Run(name, func(t *testing.T) {
			get := webTestRequest(t, app, http.MethodGet, "/assets/"+name, nil)
			if get.Code != http.StatusOK {
				t.Fatalf("GET /assets/%s = %d %s", name, get.Code, get.Body.String())
			}
			if got := get.Body.Bytes(); string(got) != string(want) {
				t.Fatalf("/assets/%s 返回字节与嵌入文件不一致", name)
			}
			if got := get.Header().Get("Content-Type"); got != contentType {
				t.Fatalf("/assets/%s Content-Type = %q，期望 %q", name, got, contentType)
			}
			if got := get.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("/assets/%s Cache-Control = %q", name, got)
			}
			etag := get.Header().Get("ETag")
			if !regexp.MustCompile(`^"[0-9a-f]{32}"$`).MatchString(etag) {
				t.Fatalf("/assets/%s ETag 格式不符: %q", name, etag)
			}

			head := webTestRequest(t, app, http.MethodHead, "/assets/"+name, nil)
			if head.Code != http.StatusOK || head.Body.Len() != 0 {
				t.Fatalf("HEAD /assets/%s = %d，响应体 %d 字节", name, head.Code, head.Body.Len())
			}
			if head.Header().Get("Content-Length") != strconv.Itoa(len(want)) {
				t.Fatalf("HEAD /assets/%s Content-Length = %q，期望 %d", name, head.Header().Get("Content-Length"), len(want))
			}

			notModified := webTestRequest(t, app, http.MethodGet, "/assets/"+name, map[string]string{"If-None-Match": etag})
			if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
				t.Fatalf("带匹配 ETag 的请求应 304: %d，响应体 %d 字节", notModified.Code, notModified.Body.Len())
			}
			served++
		})
	}
	if served == 0 {
		t.Fatal("嵌入产物没有任何可服务资源")
	}
}

// TestWebIndexAssetReferencesAreServed ties the committed HTML to the embedded
// files: a stale or partial dist (HTML referencing a previous hash) fails here
// instead of shipping a page whose scripts 404.
func TestWebIndexAssetReferencesAreServed(t *testing.T) {
	app := webTestApp(t)
	shell := webTestEmbeddedFile(t, "dist/index.html")

	references := regexp.MustCompile(`/assets/[A-Za-z0-9._-]+\.(js|css|svg)`).FindAllString(string(shell), -1)
	if len(references) == 0 {
		t.Fatal("dist/index.html 没有引用任何 /assets/ 资源")
	}
	for _, reference := range references {
		response := webTestRequest(t, app, http.MethodGet, reference, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("index.html 引用的 %s 不可用: %d", reference, response.Code)
		}
		if response.Body.Len() == 0 {
			t.Fatalf("index.html 引用的 %s 返回空内容", reference)
		}
	}
}

func TestWebAssetsRejectTraversalAndUnknownNames(t *testing.T) {
	app := webTestApp(t)
	shell := webTestEmbeddedFile(t, "dist/index.html")

	cases := []struct {
		name   string
		target string
	}{
		{"点开头", "/assets/.hidden.js"},
		{"不在白名单的扩展名", "/assets/index.html"},
		{"旧预览页资源名", "/assets/app.css"},
		{"不存在的哈希文件", "/assets/index-00000000.js"},
		{"多段路径", "/assets/nested/app.js"},
		{"上跳未编码", "/assets/../index.html"},
		{"上跳编码斜杠", "/assets/%2e%2e%2findex.html"},
		{"上跳编码点", "/assets/%2e%2e/index.html"},
		{"反斜杠", "/assets/..%5cindex.html"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := webTestRequest(t, app, http.MethodGet, testCase.target, nil)
			if response.Code == http.StatusOK {
				t.Fatalf("%s 不应返回 200: %d", testCase.target, response.Code)
			}
			if body := response.Body.String(); strings.Contains(body, "<!doctype html") && body == string(shell) {
				t.Fatalf("%s 泄露了页面 shell", testCase.target)
			}
		})
	}

	// 写方法不会落到资源读取。单 ServeMux 拓扑下，"路径匹配但方法不匹配" 由 ServeMux
	// 直接返回 405（Allow 只列 GET/HEAD），不会返回资源字节或页面 shell。
	post := webTestRequest(t, app, http.MethodPost, "/assets/index.html", nil)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /assets/index.html 应 405: %d", post.Code)
	}
	if body := post.Body.String(); strings.Contains(body, "<!doctype html") || len(body) > 64 {
		t.Fatalf("POST /assets/index.html 不应返回内容: %q", body)
	}
	if allow := post.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("405 响应应带 Allow: GET，实际 %q", allow)
	}
}

// TestWebImagePageOnlyServesPlausibleIDs covers the identifier shape check. The
// document status of an existing record is covered by
// TestWebImagePageStatusFollowsSharedVisibility, which needs a database.
func TestWebImagePageOnlyServesPlausibleIDs(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	shell := webTestEmbeddedFile(t, "dist/index.html")

	// 形状合法但不存在的 id 是一张不可见的图片：状态 404，仍返回 404 页壳。
	missing := env.do(t, http.MethodGet, "/images/abc-123_ok.id~", nil, "", imgTestAuth{})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的合法 id 应 404: %d", missing.Code)
	}
	if missing.Body.String() != string(shell) {
		t.Fatalf("不存在的图片详情仍应返回 404 页壳")
	}

	tooLong := "/images/" + strings.Repeat("a", 65)
	for _, target := range []string{tooLong, "/images/%20", "/images/..", "/images/a%2fb"} {
		response := env.do(t, http.MethodGet, target, nil, "", imgTestAuth{})
		if response.Code == http.StatusOK {
			t.Fatalf("%s 不应返回页面 shell: %d", target, response.Code)
		}
		if response.Header().Get("Location") != "" && response.Code == http.StatusPermanentRedirect {
			t.Fatalf("%s 不应被当成图片详情做尾斜杠跳转", target)
		}
	}
}

// TestWebImagePageStatusFollowsSharedVisibility pins the HTTP status of the
// detail page to the same visibility rule /api/images/{id}/meta uses: 200 for a
// visible image, 404 for a missing, trashed or hidden one, with the uploader's
// own cookie as the only way to still see a hidden image.
func TestWebImagePageStatusFollowsSharedVisibility(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	shell := webTestEmbeddedFile(t, "dist/index.html")
	owner := env.registerAdmin(t, "page-owner@example.com", "password123")
	ownerAuth := imgTestCookieAuth(owner)
	_, otherCookie := env.addUser(t, "page-other@example.com")
	otherAuth := imgTestCookieAuth(otherCookie)

	imageID, _ := imgTrashTestUpload(t, env, ownerAuth, "detail.png", []string{"detail"})
	page := func(auth imgTestAuth, method, id string) *httptest.ResponseRecorder {
		return env.do(t, method, "/images/"+id, nil, "", auth)
	}
	assertPage := func(label string, auth imgTestAuth, method, id string, want int) *httptest.ResponseRecorder {
		t.Helper()
		response := page(auth, method, id)
		if response.Code != want {
			t.Fatalf("%s: %s /images/%s = %d，期望 %d", label, method, id, response.Code, want)
		}
		if want == http.StatusOK || want == http.StatusNotFound {
			if method == http.MethodHead {
				if response.Body.Len() != 0 {
					t.Fatalf("%s: HEAD 不应返回响应体（%d 字节）", label, response.Body.Len())
				}
				if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(shell)) {
					t.Fatalf("%s: HEAD Content-Length = %q，页面壳 %d 字节", label, got, len(shell))
				}
			} else if response.Body.String() != string(shell) {
				t.Fatalf("%s: 详情页应返回同一份页面壳", label)
			}
		}
		return response
	}

	// 可见图片：任何人都拿到 200 + 页面壳。
	for name, auth := range map[string]imgTestAuth{
		"anonymous":    {},
		"owner":        ownerAuth,
		"other-cookie": otherAuth,
	} {
		assertPage(name+" 可见图片", auth, http.MethodGet, imageID, http.StatusOK)
		assertPage(name+" 可见图片", auth, http.MethodHead, imageID, http.StatusOK)
	}

	// 隐藏图片：只有上传者本人的 Cookie 看到 200，其他人（含 API Key）一律 404。
	apiKey := env.addAPIKey(t, 1)
	imgTestSetHidden(t, env, ownerAuth, imageID, false)
	for name, auth := range map[string]imgTestAuth{
		"anonymous":     {},
		"other-cookie":  otherAuth,
		"owner-api-key": imgTestBearerAuth(apiKey),
	} {
		response := assertPage(name+" 隐藏图片", auth, http.MethodGet, imageID, http.StatusNotFound)
		if response.Header().Get("ETag") != "" {
			t.Fatalf("%s 的 404 不应带 ETag（304 不能代表「不存在」）", name)
		}
		assertPage(name+" 隐藏图片", auth, http.MethodHead, imageID, http.StatusNotFound)
	}
	assertPage("上传者本人", ownerAuth, http.MethodGet, imageID, http.StatusOK)
	// 恢复可见后其他人重新看到 200。
	imgTestSetHidden(t, env, ownerAuth, imageID, true)
	assertPage("恢复可见", otherAuth, http.MethodGet, imageID, http.StatusOK)

	// 回收站与管理端下架的记录对上传者也是 404；恢复后回到 200。
	imgTestTrash(t, env, ownerAuth, imageID)
	assertPage("回收站", ownerAuth, http.MethodGet, imageID, http.StatusNotFound)
	imgTrashTestRestore(t, env, ownerAuth, imageID)
	assertPage("回收站恢复", ownerAuth, http.MethodGet, imageID, http.StatusOK)
	if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedAt = ?, deletedByAdmin = 1 WHERE id = ?`, imagesNow(), imageID); err != nil {
		t.Fatalf("模拟管理员下架失败: %v", err)
	}
	assertPage("管理员下架", ownerAuth, http.MethodGet, imageID, http.StatusNotFound)
	assertPage("管理员下架", ownerAuth, http.MethodHead, imageID, http.StatusNotFound)

	// 不存在的记录同样是 404 + 页壳。
	assertPage("缺失记录", ownerAuth, http.MethodGet, uuid.NewString(), http.StatusNotFound)
}

// TestWebImagePageReportsBackendFailureAsServerError keeps a database failure
// from looking like "this image does not exist": the shell still loads (the page
// requests /meta itself) but the document status is 500.
func TestWebImagePageReportsBackendFailureAsServerError(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	shell := webTestEmbeddedFile(t, "dist/index.html")
	if err := env.app.db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	response := env.do(t, http.MethodGet, "/images/"+uuid.NewString(), nil, "", imgTestAuth{})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("数据库不可用时详情页 status = %d，期望 500", response.Code)
	}
	if response.Body.String() != string(shell) {
		t.Fatalf("数据库不可用时仍应返回页面壳（页面自己报告失败）")
	}
}

// TestWebPageRoutesCanonicaliseTrailingSlashLikeNext pins the Next.js
// trailingSlash:false behaviour: a registered page route with a trailing slash
// answers 308 to the canonical path and keeps the query string.
func TestWebPageRoutesCanonicaliseTrailingSlashLikeNext(t *testing.T) {
	app := webTestApp(t)

	cases := []struct {
		target   string
		location string
	}{
		{"/dashboard/", "/dashboard"},
		{"/dashboard/?tab=trash&tag=a%2Fb&empty=", "/dashboard?tab=trash&tag=a%2Fb&empty="},
		{"/admin/", "/admin"},
		{"/admin/?page=2", "/admin?page=2"},
		{"/forgot-password/?email=a%40b.test", "/forgot-password?email=a%40b.test"},
		{"/reset-password/", "/reset-password"},
		{"/verify-email/?token=abc", "/verify-email?token=abc"},
		{"/images/abc-123/", "/images/abc-123"},
		{"/images/abc-123/?from=share", "/images/abc-123?from=share"},
	}
	for _, testCase := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			response := webTestRequest(t, app, method, testCase.target, nil)
			if response.Code != http.StatusPermanentRedirect {
				t.Fatalf("%s %s = %d，期望 308", method, testCase.target, response.Code)
			}
			if got := response.Header().Get("Location"); got != testCase.location {
				t.Fatalf("%s %s Location = %q，期望 %q", method, testCase.target, got, testCase.location)
			}
			if response.Body.Len() != 0 {
				t.Fatalf("%s %s 的 308 不应带响应体: %q", method, testCase.target, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("%s %s Cache-Control = %q", method, testCase.target, got)
			}
		}
	}

	// 未知路径上的 \"//\" 仍由 ServeMux 清理并 307，不被兜底换成 404 页。
	for _, testCase := range []struct{ target, location string }{
		{"/nope//x", "/nope/x"},
		{"/dashboard//", "/dashboard/"},
	} {
		response := webTestRequest(t, app, http.MethodGet, testCase.target, nil)
		if response.Code != http.StatusTemporaryRedirect {
			t.Fatalf("%s = %d，期望 ServeMux 的 307 清理跳转", testCase.target, response.Code)
		}
		if got := response.Header().Get("Location"); got != testCase.location {
			t.Fatalf("%s Location = %q，期望 %q", testCase.target, got, testCase.location)
		}
	}

	// 根路径本来就规范，未知路径在原版直接渲染 404（不跳转），两者都不做 308。
	for _, target := range []string{"/", "/nope/", "/images/"} {
		response := webTestRequest(t, app, http.MethodGet, target, nil)
		if response.Code == http.StatusPermanentRedirect {
			t.Fatalf("%s 不应被尾斜杠跳转: Location %q", target, response.Header().Get("Location"))
		}
	}
}

func TestWebConfigReportsRuntimeUploadLimit(t *testing.T) {
	app := webTestApp(t)

	cases := []struct {
		name     string
		config   int64
		expected int64
	}{
		{"10MiB", 10 << 20, 10 << 20},
		{"512KiB", 512 << 10, 512 << 10},
		{"未配置时沿用 5MiB 默认值", 0, 5 << 20},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app.cfg = Config{UploadMaxBytes: testCase.config}
			response := webTestRequest(t, app, http.MethodGet, "/api/config", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("/api/config status = %d: %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("/api/config Content-Type = %q", got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("/api/config 必须 no-store，实际 %q", got)
			}

			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("/api/config 不是合法 JSON: %v", err)
			}
			if len(body) != 3 {
				t.Fatalf("/api/config 只应暴露 ok、uploadMaxBytes 与 mailEnabled，实际 %v", body)
			}
			if ok, _ := body["ok"].(bool); !ok {
				t.Fatalf("/api/config ok = %v", body["ok"])
			}
			// 本用例没有 SMTP 与 BASEURL：邮件能力必须明确为 false，而不是缺失字段。
			if enabled, isBool := body["mailEnabled"].(bool); !isBool || enabled {
				t.Fatalf("/api/config mailEnabled = %v（未配置 SMTP 时必须是布尔 false）", body["mailEnabled"])
			}
			value, isNumber := body["uploadMaxBytes"].(float64)
			if !isNumber || int64(value) != testCase.expected {
				t.Fatalf("/api/config uploadMaxBytes = %v，期望 %d", body["uploadMaxBytes"], testCase.expected)
			}
		})
	}
}

func TestWebConfigReportsMailCapability(t *testing.T) {
	baseURL := "https://picbox.example.com"
	smtp := SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587,
		Secure:   false,
		Username: "mailer@example.com",
		Password: "super-secret-password",
		From:     "no-reply@example.com",
	}

	cases := []struct {
		name    string
		smtp    SMTPConfig
		baseURL string
		want    bool
	}{
		{"未配置 SMTP", SMTPConfig{}, baseURL, false},
		{"SMTP 完整但缺少 BASEURL", smtp, "", false},
		{"SMTP 与 BASEURL 都齐全", smtp, baseURL, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app := webTestApp(t)
			app.cfg = Config{SMTP: testCase.smtp, BaseURL: testCase.baseURL}

			response := webTestRequest(t, app, http.MethodGet, "/api/config", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("/api/config status = %d: %s", response.Code, response.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("/api/config 不是合法 JSON: %v", err)
			}
			enabled, isBool := body["mailEnabled"].(bool)
			if !isBool {
				t.Fatalf("/api/config mailEnabled 必须是布尔值，实际 %v", body["mailEnabled"])
			}
			if enabled != testCase.want {
				t.Fatalf("/api/config mailEnabled = %v，期望 %v", enabled, testCase.want)
			}

			// 能力标志只能是布尔值：SMTP 主机、端口、账号、密码与发件人都不得出现在
			// 公开响应里，连字段名都不行。
			for _, forbidden := range []string{
				"smtp", "smtpHost", "smtpPort", "smtpUser", "smtpPassword", "smtpFrom",
				"host", "port", "username", "user", "password", "from",
			} {
				if _, exists := body[forbidden]; exists {
					t.Fatalf("/api/config 不应暴露字段 %q（实际 %v）", forbidden, body)
				}
			}
			raw := response.Body.String()
			for _, secret := range []string{
				smtp.Host, smtp.Username, smtp.Password, smtp.From,
			} {
				if strings.Contains(raw, secret) {
					t.Fatalf("/api/config 响应里出现了 SMTP 细节 %q", secret)
				}
			}
		})
	}
}

func TestWebConfigIsNotSwallowedByPageOrAPIRouters(t *testing.T) {
	app := webTestApp(t)
	app.cfg = Config{UploadMaxBytes: 3 << 20}

	// 与页面路由同名前缀，但它是 JSON 接口，不能被返回成 HTML 壳。
	response := webTestRequest(t, app, http.MethodGet, "/api/config", nil)
	if strings.Contains(response.Body.String(), "<!doctype html") {
		t.Fatalf("/api/config 不应返回页面: %s", response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("/api/config Content-Type = %q", got)
	}

	// HEAD 的响应体由真实 HTTP 服务器丢弃；ResponseRecorder 不会，因此用 httptest.Server
	// 验证一次线上行为（本机环回，不启动预览服务）。
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	request, err := http.NewRequest(http.MethodHead, server.URL+"/api/config", nil)
	if err != nil {
		t.Fatalf("构造 HEAD 请求失败: %v", err)
	}
	headResponse, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("HEAD /api/config 失败: %v", err)
	}
	defer headResponse.Body.Close()
	headBody, err := io.ReadAll(headResponse.Body)
	if err != nil {
		t.Fatalf("读取 HEAD 响应失败: %v", err)
	}
	if headResponse.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Fatalf("HEAD /api/config = %d，响应体 %d 字节", headResponse.StatusCode, len(headBody))
	}
	if got := headResponse.Header.Get("Content-Length"); got != strconv.Itoa(len(response.Body.Bytes())) {
		t.Fatalf("HEAD /api/config Content-Length = %q，GET 响应体 %d 字节", got, response.Body.Len())
	}
}

// TestWebUnknownPathsServeNotFoundShell locks the item-2 contract: a navigation
// to a route this UI does not implement answers HTTP 404 and still serves the
// same shell, so the client renders the original not-found page. API and asset
// requests keep their own bodies and never receive the shell.
func TestWebUnknownPathsServeNotFoundShell(t *testing.T) {
	app := webTestApp(t)
	shell := webTestEmbeddedFile(t, "dist/index.html")

	for _, target := range []string{"/nope", "/login", "/images", "/assets", "/nope/", "/images/a/b", "/UI-Audit-Unknown",
		"/images/", "/images/%25", "/nope?tab=trash"} {
		t.Run(target, func(t *testing.T) {
			response := webTestRequest(t, app, http.MethodGet, target, nil)
			if response.Code != http.StatusNotFound {
				t.Fatalf("%s 应 404: %d", target, response.Code)
			}
			if response.Body.String() != string(shell) {
				t.Fatalf("%s 应以 404 提供同一份 404 页壳", target)
			}
			header := response.Header()
			if got := header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("%s Content-Type = %q", target, got)
			}
			if got := header.Get("Cache-Control"); got != "no-store" {
				t.Fatalf("%s Cache-Control = %q", target, got)
			}
			if header.Get("Content-Security-Policy") != webContentSecurityPolicy {
				t.Fatalf("%s 缺少统一的 CSP", target)
			}
			if got := header.Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("%s X-Frame-Options = %q", target, got)
			}
			if got := header.Get("Content-Length"); got != strconv.Itoa(len(shell)) {
				t.Fatalf("%s Content-Length = %q，页面壳 %d 字节", target, got, len(shell))
			}
			// 404 页面不能用缓存验证器：304 不能被理解成"这个路由存在"。
			if etag := header.Get("ETag"); etag != "" {
				t.Fatalf("%s 的 404 不应带 ETag: %q", target, etag)
			}
			conditional := webTestRequest(t, app, http.MethodGet, target, map[string]string{
				"If-None-Match": `"` + strings.Repeat("0", 32) + `"`,
			})
			if conditional.Code != http.StatusNotFound {
				t.Fatalf("%s 带 If-None-Match 仍应是 404: %d", target, conditional.Code)
			}

			head := webTestRequest(t, app, http.MethodHead, target, nil)
			if head.Code != http.StatusNotFound || head.Body.Len() != 0 {
				t.Fatalf("HEAD %s = %d，响应体 %d 字节", target, head.Code, head.Body.Len())
			}
			if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(shell)) {
				t.Fatalf("HEAD %s Content-Length = %q，页面壳 %d 字节", target, got, len(shell))
			}
		})
	}

	// 资源探测不是导航：/assets/* 未命中仍是空 404，不回页面壳。
	for _, target := range []string{"/assets/.hidden.js", "/assets/index.html", "/assets/nested/app.js", "/assets/"} {
		response := webTestRequest(t, app, http.MethodGet, target, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404: %d", target, response.Code)
		}
		if response.Body.String() == string(shell) {
			t.Fatalf("%s 不应返回页面壳", target)
		}
		if strings.Contains(response.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s 不应按页面渲染: %q", target, response.Header().Get("Content-Type"))
		}
	}

	// 未知 /api/ 仍然走 JSON 错误，不会被页面路由吞掉。
	api := webTestRequest(t, app, http.MethodGet, "/api/definitely-not-implemented", nil)
	if api.Code != http.StatusNotFound {
		t.Fatalf("未知 API 应 404: %d", api.Code)
	}
	if got := api.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("未知 API Content-Type = %q", got)
	}
	if body := api.Body.String(); !strings.Contains(body, `"ok":false`) || !strings.Contains(body, `"code":"NOT_FOUND"`) {
		t.Fatalf("未知 API 应返回 JSON 错误: %s", body)
	}

	// 写方法不拿页面壳，也不改变原有方法语义：页面路由（只注册了 GET）仍是
	// 405 + Allow: GET, HEAD；未知路径仍是纯文本 404。
	methodCases := []struct {
		method string
		target string
		status int
		allow  string
	}{
		{http.MethodPost, "/dashboard", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPost, "/dashboard/", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPut, "/admin", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodDelete, "/images/abc-123", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPost, "/assets/index.html", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPost, "/", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPost, "/nope", http.StatusNotFound, ""},
		{http.MethodDelete, "/images/a/b", http.StatusNotFound, ""},
		{http.MethodPost, "/assets", http.StatusNotFound, ""},
		// 多段资产名与非法图片 ID 的方法行为保持 ServeMux 原样：前者本来就没有
		// 匹配的模式（404），后者匹配 "GET /images/{id}" 但方法不符（405 + Allow）。
		{http.MethodPost, "/assets/nested/app.js", http.StatusNotFound, ""},
		{http.MethodPost, "/images/%20", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPut, "/images/" + strings.Repeat("a", 65), http.StatusMethodNotAllowed, "GET, HEAD"},
	}
	for _, testCase := range methodCases {
		response := webTestRequest(t, app, testCase.method, testCase.target, nil)
		if response.Code != testCase.status {
			t.Fatalf("%s %s = %d，期望 %d", testCase.method, testCase.target, response.Code, testCase.status)
		}
		if got := response.Header().Get("Allow"); got != testCase.allow {
			t.Fatalf("%s %s Allow = %q，期望 %q", testCase.method, testCase.target, got, testCase.allow)
		}
		if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("%s %s 不应返回页面内容: %q", testCase.method, testCase.target, got)
		}
		if response.Body.String() == string(shell) {
			t.Fatalf("%s %s 不应返回页面壳", testCase.method, testCase.target)
		}
	}
}
