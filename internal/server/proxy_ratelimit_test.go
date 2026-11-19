package server

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 可信代理（TRUSTED_PROXIES）、限流分桶与上传频率桶的回归 ----
// 这些用例都走真实的 httptest 请求/Config，不依赖外部服务。

func proxyTestApp(t *testing.T, trusted string) *App {
	t.Helper()
	prefixes, err := parseTrustedProxies(trusted)
	if err != nil {
		t.Fatalf("解析 TRUSTED_PROXIES %q 失败: %v", trusted, err)
	}
	return coreTestApp(t, Config{TrustedProxies: prefixes})
}

func proxyTestRequest(t *testing.T, remoteAddr string, forwarded []string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/password", strings.NewReader("{}"))
	request.RemoteAddr = remoteAddr
	for _, value := range forwarded {
		request.Header.Add("X-Forwarded-For", value)
	}
	return request
}

func TestClientIPIgnoresForwardedHeadersWithoutTrustedProxy(t *testing.T) {
	app := proxyTestApp(t, "")

	request := proxyTestRequest(t, "198.51.100.10:4321", []string{"203.0.113.7"})
	request.Header.Set("X-Real-IP", "203.0.113.99")
	if got := app.clientIP(request); got != "198.51.100.10" {
		t.Fatalf("未配置可信代理时不能读 XFF/X-Real-IP: %q", got)
	}
	if got := clientIP(request); got != "198.51.100.10" {
		t.Fatalf("直连 helper 也必须忽略转发头: %q", got)
	}

	// 配置了可信代理范围，但直连对端不在范围内：同样忽略所有转发头。
	loopbackOnly := proxyTestApp(t, "127.0.0.1/32")
	spoofed := proxyTestRequest(t, "198.51.100.10:4321", []string{"203.0.113.7"})
	spoofed.Header.Set("X-Real-IP", "203.0.113.99")
	if got := loopbackOnly.clientIP(spoofed); got != "198.51.100.10" {
		t.Fatalf("非可信直连对端的 XFF 不能改变分桶: %q", got)
	}
}

func TestClientIPUsesRightmostUntrustedAddressOfTrustedProxy(t *testing.T) {
	app := proxyTestApp(t, "127.0.0.1/32,::1/128")

	cases := []struct {
		name      string
		remote    string
		forwarded []string
		want      string
	}{
		{
			// 恶意左前缀（客户端自带）不能覆盖可信代理追加的真实地址。
			name:      "左侧伪造前缀被忽略",
			remote:    "127.0.0.1:5555",
			forwarded: []string{"203.0.113.9, 198.51.100.4"},
			want:      "198.51.100.4",
		},
		{
			name:      "多个伪造前缀",
			remote:    "127.0.0.1:5555",
			forwarded: []string{"203.0.113.1, 203.0.113.2, 203.0.113.3, 198.51.100.4"},
			want:      "198.51.100.4",
		},
		{
			name:   "没有 XFF 时用直连对端",
			remote: "127.0.0.1:5555",
			want:   "127.0.0.1",
		},
		{
			// X-Real-IP 是单值头，可被覆盖/伪造，不参与判定。
			name:      "只有 X-Real-IP 时用直连对端",
			remote:    "127.0.0.1:5555",
			forwarded: nil,
			want:      "127.0.0.1",
		},
		{
			name:      "多个同名头按一条链处理",
			remote:    "[::1]:5555",
			forwarded: []string{"203.0.113.9", "198.51.100.4"},
			want:      "198.51.100.4",
		},
		{
			name:      "IPv6 客户端规范化",
			remote:    "[::1]:5555",
			forwarded: []string{"2001:0DB8::0001"},
			want:      "2001:db8::1",
		},
		{
			name:      "v4-mapped 客户端还原为 IPv4",
			remote:    "[::ffff:127.0.0.1]:5555",
			forwarded: []string{"::ffff:198.51.100.4"},
			want:      "198.51.100.4",
		},
	}
	for _, table := range cases {
		t.Run(table.name, func(t *testing.T) {
			request := proxyTestRequest(t, table.remote, table.forwarded)
			request.Header.Set("X-Real-IP", "203.0.113.99")
			if got := app.clientIP(request); got != table.want {
				t.Fatalf("clientIP = %q，期望 %q（XFF=%v）", got, table.want, table.forwarded)
			}
		})
	}
}

func TestClientIPStripsEveryExplicitlyTrustedProxy(t *testing.T) {
	app := proxyTestApp(t, "127.0.0.1/32,10.0.0.0/8")

	// 对端与最右跳都是可信代理：继续往左剥离，第一个不可信地址才是客户端。
	multi := proxyTestRequest(t, "127.0.0.1:5555", []string{"203.0.113.9, 10.1.2.3"})
	if got := app.clientIP(multi); got != "203.0.113.9" {
		t.Fatalf("多级可信代理剥离结果应为 203.0.113.9: %q", got)
	}

	// 整条链都在可信范围内：最左项就是客户端。
	allTrusted := proxyTestRequest(t, "127.0.0.1:5555", []string{"10.9.9.9, 10.1.2.3"})
	if got := app.clientIP(allTrusted); got != "10.9.9.9" {
		t.Fatalf("整条链可信时应取最左项: %q", got)
	}

	// 中间跳不可信：一旦遇到不可信地址就停下，它的左侧不再被采用。
	mixed := proxyTestRequest(t, "127.0.0.1:5555", []string{"203.0.113.9, 198.51.100.4, 10.1.2.3"})
	if got := app.clientIP(mixed); got != "198.51.100.4" {
		t.Fatalf("首个不可信地址应为客户端: %q", got)
	}
}

func TestClientIPRejectsInvalidForwardedChain(t *testing.T) {
	app := proxyTestApp(t, "127.0.0.1/32")

	tooManyHops := make([]string, 0, maxForwardedHops+1)
	for i := 0; i <= maxForwardedHops; i++ {
		tooManyHops = append(tooManyHops, "198.51.100.4")
	}

	cases := map[string][]string{
		"主机名":      {"proxy.example.com"},
		"带端口":      {"198.51.100.4:8080"},
		"方括号 IPv6": {"[2001:db8::1]"},
		"带 zone":   {"fe80::1%eth0"},
		"未知":       {"unknown"},
		"尾部空段":     {"198.51.100.4,"},
		"首部空段":     {",198.51.100.4"},
		"空白值":      {""},
		"超长单项":     {strings.Repeat("1", maxForwardedEntrySize+1)},
		"跳数超界":     tooManyHops,
	}
	for name, forwarded := range cases {
		t.Run(name, func(t *testing.T) {
			request := proxyTestRequest(t, "127.0.0.1:5555", forwarded)
			if got := app.clientIP(request); got != "127.0.0.1" {
				t.Fatalf("无效 XFF 链必须退回直连对端，实际 %q（XFF=%v）", got, forwarded)
			}
		})
	}
}

// TestPasswordRateLimitUsesTrustedClientIP 用改密接口验证“按真实客户端分桶”：
// 该接口在读取 Cookie/密码前先查 IP 桶，适合在真实 HTTP 路径上做大次数断言。
func TestPasswordRateLimitUsesTrustedClientIP(t *testing.T) {
	app := proxyTestApp(t, "127.0.0.1/32")
	handler := app.Handler()

	attempt := func(forwarded string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/password",
			strings.NewReader(`{"currentPassword":"current123","newPassword":"next123"}`))
		request.RemoteAddr = "127.0.0.1:54321"
		request.Header.Set("Content-Type", "application/json")
		if forwarded != "" {
			request.Header.Set("X-Forwarded-For", forwarded)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	for i := 0; i < passwordChangeIPLimit; i++ {
		if recorder := attempt("203.0.113.1"); recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求不应被限流", i+1)
		}
	}
	limited := attempt("203.0.113.1")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("同一客户端的第 %d 次请求应 429: %d %s",
			passwordChangeIPLimit+1, limited.Code, limited.Body.String())
	}
	if code := coreErrorCode(t, &coreResponse{status: limited.Code, body: imgTestJSON(t, limited)}); code != "RATE_LIMITED" {
		t.Fatalf("错误码应为 RATE_LIMITED: %q", code)
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Fatalf("限流响应必须带 Retry-After")
	}

	// 另一个真实客户端独立分桶，不能受上面 10 次的影响。
	if recorder := attempt("203.0.113.2"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("不同客户端应独立分桶（期望 401 未登录）: %d %s", recorder.Code, recorder.Body.String())
	}

	// 客户端伪造的左前缀不能换桶：真实客户端仍是 203.0.113.1。
	if recorder := attempt("203.0.113.77, 203.0.113.1"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("伪造左前缀不能改写限流桶: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRateLimitIgnoresForwardedHeadersWhenProxyUntrusted(t *testing.T) {
	// 默认部署（无 TRUSTED_PROXIES）：同一对端换 XFF 仍共用桶。
	app := coreTestApp(t, Config{})
	handler := app.Handler()

	attempt := func(forwarded string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/password",
			strings.NewReader(`{"currentPassword":"current123","newPassword":"next123"}`))
		request.RemoteAddr = "198.51.100.20:1234"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Forwarded-For", forwarded)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	limited := false
	for i := 0; i < passwordChangeIPLimit+2; i++ {
		if attempt("203.0.113."+string(rune('0'+i%10))) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("未配置可信代理时，伪造 XFF 不能产生新桶")
	}
}

// ---- 上传频率桶（原版 limiterUpload：100 次/小时/IP） ----

func proxyUploadLimitAttempt(t *testing.T, app *App, remoteAddr, forwarded string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/images", strings.NewReader(""))
	request.RemoteAddr = remoteAddr
	if forwarded != "" {
		request.Header.Set("X-Forwarded-For", forwarded)
	}
	recorder := httptest.NewRecorder()
	app.limitImageUpload(recorder, request)
	return recorder
}

func TestImageUploadLimiterCountsPerClientIP(t *testing.T) {
	app := coreTestApp(t, Config{})

	for i := 0; i < uploadImageRateLimit; i++ {
		recorder := proxyUploadLimitAttempt(t, app, "192.0.2.7:1234", "")
		if recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("前 %d 次上传不应被限流（第 %d 次）", uploadImageRateLimit, i+1)
		}
	}
	limited := proxyUploadLimitAttempt(t, app, "192.0.2.7:1234", "")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次上传应 429: %d %s", uploadImageRateLimit+1, limited.Code, limited.Body.String())
	}
	body := imgTestJSON(t, limited)
	if code := coreErrorCode(t, &coreResponse{status: limited.Code, body: body}); code != "RATE_LIMITED" {
		t.Fatalf("上传限流错误码应为 RATE_LIMITED: %q", code)
	}
	if retryAfter := limited.Header().Get("Retry-After"); retryAfter != "3600" {
		t.Fatalf("上传限流应带 1 小时的 Retry-After，实际 %q", retryAfter)
	}
	if retryAfterMs, ok := body["retryAfterMs"].(float64); !ok || retryAfterMs <= 0 {
		t.Fatalf("限流响应应包含 retryAfterMs: %v", body)
	}

	// 另一个客户端 IP 独立分桶。
	if recorder := proxyUploadLimitAttempt(t, app, "192.0.2.8:1234", ""); recorder.Code != http.StatusOK {
		t.Fatalf("不同客户端应独立分桶: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestImageUploadLimiterFollowsTrustedProxyClientIP(t *testing.T) {
	app := proxyTestApp(t, "127.0.0.1/32")

	for i := 0; i < uploadImageRateLimit; i++ {
		proxyUploadLimitAttempt(t, app, "127.0.0.1:5000", "203.0.113.1")
	}
	if recorder := proxyUploadLimitAttempt(t, app, "127.0.0.1:5000", "203.0.113.1"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("同一真实客户端应被限流: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := proxyUploadLimitAttempt(t, app, "127.0.0.1:5000", "203.0.113.2"); recorder.Code != http.StatusOK {
		t.Fatalf("反代后的不同真实客户端应独立分桶: %d %s", recorder.Code, recorder.Body.String())
	}
	// 伪造左前缀不能让已限流的客户端换桶。
	if recorder := proxyUploadLimitAttempt(t, app, "127.0.0.1:5000", "203.0.113.99, 203.0.113.1"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("伪造左前缀不能换桶: %d %s", recorder.Code, recorder.Body.String())
	}
}

// proxyTestMultipart 构造一个真实 multipart 上传体（保持原版字段名 file）。
func proxyTestMultipart(t *testing.T, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile("file", "tiny.png")
	if err != nil {
		t.Fatalf("创建文件部件失败: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("写入文件部件失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("结束 multipart 失败: %v", err)
	}
	return &buffer, writer.FormDataContentType()
}

func proxyTestPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("生成测试 PNG 失败: %v", err)
	}
	return buffer.Bytes()
}

// proxyCountingBody 记录服务端实际读取了多少请求体字节。
type proxyCountingBody struct {
	reader io.Reader
	read   int
}

func (b *proxyCountingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *proxyCountingBody) Close() error { return nil }

// TestUploadRateLimitSharedBucketBetweenMultipartAndURLImport 验证 multipart 上传与
// URL 导入（调用同一个 a.limitImageUpload）共用 100 次/小时/IP 的桶，且超限请求
// 在读取请求体之前就被拒绝：不读 body、不建临时文件、不解码、不写数据库。
func TestUploadRateLimitSharedBucketBetweenMultipartAndURLImport(t *testing.T) {
	dataDir := t.TempDir()
	app := coreTestApp(t, Config{DataDir: dataDir})
	_, cookie := coreRegister(t, app, "upload-limit@example.com", "secret123")
	handler := app.Handler()

	image := proxyTestPNG(t)

	// 1) multipart 上传成功一次，先证明它确实消耗了同一个桶。
	firstBody, firstContentType := proxyTestMultipart(t, image)
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/images", firstBody)
	firstRequest.Header.Set("Content-Type", firstContentType)
	firstRequest.AddCookie(cookie)
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, firstRequest)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("首次上传应成功: %d %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	if count := proxyTestImageCount(t, app); count != 1 {
		t.Fatalf("首次上传后应只有 1 张图片，实际 %d", count)
	}

	// 2) 通过 from-url 会用的同一 helper 再消耗 99 次：总计正好 100 次。
	for i := 0; i < uploadImageRateLimit-1; i++ {
		if recorder := proxyUploadLimitAttempt(t, app, "192.0.2.1:1234", ""); recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求不应被限流（multipart 与 URL 导入应共用桶）", i+2)
		}
	}

	// 3) 第 101 次（multipart）必须 429，且在读取 body 之前返回。
	body, contentType := proxyTestMultipart(t, image)
	counting := &proxyCountingBody{reader: body}
	request := httptest.NewRequest(http.MethodPost, "/api/images", counting)
	request.Header.Set("Content-Type", contentType)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("超限上传应 429: %d %s", recorder.Code, recorder.Body.String())
	}
	bodyJSON := imgTestJSON(t, recorder)
	if code := coreErrorCode(t, &coreResponse{status: recorder.Code, body: bodyJSON}); code != "RATE_LIMITED" {
		t.Fatalf("上传限流错误码应为 RATE_LIMITED: %q", code)
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != "3600" {
		t.Fatalf("上传限流应带 1 小时 Retry-After，实际 %q", retryAfter)
	}
	if counting.read != 0 {
		t.Fatalf("被限流的上传不得读取请求体，实际读取 %d 字节", counting.read)
	}
	if held := len(app.uploadSlots); held != 0 {
		t.Fatalf("被限流的上传不应占用单处理槽，当前占用 %d", held)
	}

	// 4) 无副作用：不落临时文件、不新增图片/字节（也就是没有解码与存储）。
	if entries := proxyTestTempEntries(t, dataDir); len(entries) != 0 {
		t.Fatalf("被限流的上传不应留下临时文件: %v", entries)
	}
	if count := proxyTestImageCount(t, app); count != 1 {
		t.Fatalf("被限流的上传不应写入图片记录，实际 %d", count)
	}
	if entries := imgTestImagesDirEntries(t, dataDir); len(entries) == 0 {
		t.Fatalf("首次成功的上传应有落盘文件")
	}
}

// TestUploadRateLimitRejectsBeforeDecoding 用非法图片字节证明 429 发生在格式校验之前。
func TestUploadRateLimitRejectsBeforeDecoding(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "upload-limit-decode@example.com", "secret123")

	for i := 0; i < uploadImageRateLimit; i++ {
		proxyUploadLimitAttempt(t, app, "192.0.2.1:1234", "")
	}

	body, contentType := proxyTestMultipart(t, []byte("not an image at all"))
	request := httptest.NewRequest(http.MethodPost, "/api/images", body)
	request.Header.Set("Content-Type", contentType)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("超限上传应先返回 429 而不是格式错误: %d %s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "RATE_LIMITED" {
		t.Fatalf("错误码应为 RATE_LIMITED，实际 %q（说明先解码/校验了请求体）", code)
	}
	if count := proxyTestImageCount(t, app); count != 0 {
		t.Fatalf("被限流的上传不应落库，实际 %d", count)
	}
}

// TestUploadRateLimitKeepsSingleSlotBackpressure 守住“频率桶不替代单槽背压”。
func TestUploadRateLimitKeepsSingleSlotBackpressure(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, cookie := coreRegister(t, app, "upload-slot@example.com", "secret123")

	app.uploadSlots <- struct{}{}
	defer func() { <-app.uploadSlots }()

	body, contentType := proxyTestMultipart(t, proxyTestPNG(t))
	request := httptest.NewRequest(http.MethodPost, "/api/images", body)
	request.Header.Set("Content-Type", contentType)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("单槽占用时应 429: %d %s", recorder.Code, recorder.Body.String())
	}
	if code := imgTestErrorCode(t, recorder); code != "UPLOAD_BUSY" {
		t.Fatalf("单槽满载仍应是 UPLOAD_BUSY（频率桶不替代背压），实际 %q", code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("UPLOAD_BUSY 应带 Retry-After")
	}
}

func proxyTestImageCount(t *testing.T, app *App) int {
	t.Helper()
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM "Image"`).Scan(&count); err != nil {
		t.Fatalf("统计图片失败: %v", err)
	}
	return count
}

func proxyTestTempEntries(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, imagesTempDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取上传暂存目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
