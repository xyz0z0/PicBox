package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCrossOriginWritesUseConfiguredBaseURL(t *testing.T) {
	app := coreTestApp(t, Config{BaseURL: "https://pic.example.com"})

	crossSource := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"},
		map[string]string{"Origin": "https://evil.example"})
	if crossSource.status != http.StatusForbidden || coreErrorCode(t, crossSource) != "CROSS_ORIGIN_DENIED" {
		t.Fatalf("跨源写请求必须 403: %d %v", crossSource.status, crossSource.body)
	}

	nullOrigin := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"},
		map[string]string{"Origin": "null"})
	if nullOrigin.status != http.StatusForbidden {
		t.Fatalf("Origin: null 必须拒绝: %d", nullOrigin.status)
	}

	sameOrigin := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"},
		map[string]string{"Origin": "https://pic.example.com"})
	if sameOrigin.status != http.StatusBadRequest || coreErrorCode(t, sameOrigin) != "INVALID_CREDENTIALS" {
		t.Fatalf("配置 BASEURL 的同源写请求应到达处理函数: %d %v", sameOrigin.status, sameOrigin.body)
	}

	// 读取请求不受 Origin 限制。
	read := coreDo(t, app.Handler(), http.MethodGet, "/api/health", nil,
		map[string]string{"Origin": "https://evil.example"})
	if read.status != http.StatusOK {
		t.Fatalf("读取接口不应被跨源中间件拦截: %d", read.status)
	}
}

func TestCrossOriginWritesWithoutBaseURLOnlyAllowLoopback(t *testing.T) {
	app := coreTestApp(t, Config{})

	foreign := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"},
		map[string]string{"Origin": "https://evil.example"})
	if foreign.status != http.StatusForbidden || coreErrorCode(t, foreign) != "CROSS_ORIGIN_DENIED" {
		t.Fatalf("未配置 BASEURL 时外部 Origin 必须拒绝: %d %v", foreign.status, foreign.body)
	}

	// 伪造 Host 头不能放行外部 Origin（反向代理场景）。
	request := httptest.NewRequest(http.MethodPost, "http://evil.example/api/auth/login",
		nil)
	request.Host = "evil.example"
	request.Header.Set("Origin", "http://evil.example")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("没有 BASEURL 时不能只靠 Host 头放行外部 Origin: %d %s", recorder.Code, recorder.Body.String())
	}

	loopback := coreDo(t, app.Handler(), http.MethodPost, "http://127.0.0.1:3001/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"},
		map[string]string{"Origin": "http://127.0.0.1:3001"})
	if loopback.status != http.StatusBadRequest || coreErrorCode(t, loopback) != "INVALID_CREDENTIALS" {
		t.Fatalf("环回同源写请求应到达处理函数: %d %v", loopback.status, loopback.body)
	}
}

func TestWriteRequestWithoutOriginIsAllowedForCLI(t *testing.T) {
	app := coreTestApp(t, Config{BaseURL: "https://pic.example.com"})
	response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/login",
		map[string]string{"email": "a@example.com", "password": "secret123"}, nil)
	if response.status != http.StatusBadRequest || coreErrorCode(t, response) != "INVALID_CREDENTIALS" {
		t.Fatalf("无 Origin 的 CLI 请求应到达处理函数: %d %v", response.status, response.body)
	}
}

// TestCloseStopsMailAndRetentionPromptly 覆盖统一后台生命周期：
// 邮件投递与回收站清理都启动时，Close 必须一次取消就让两者退出并及时返回，
// 不能因为任一后台任务还在等定时器而挂住。
func TestCloseStopsMailAndRetentionPromptly(t *testing.T) {
	// 拉长回收站清理的首次延迟：如果 Close 没有真正取消它，Wait 会一直等到这个定时器。
	originalDelay := imagesRetentionInitialDelay
	imagesRetentionInitialDelay = time.Hour
	defer func() { imagesRetentionInitialDelay = originalDelay }()

	app, _ := coreMailApp(t)
	coreRegister(t, app, "close-bg@example.com", "secret123")

	started := make(chan struct{})
	var once sync.Once
	app.mailSend = func(ctx context.Context, _ mailMessage) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}

	if response := coreDo(t, app.Handler(), http.MethodPost, "/api/auth/forgot-password",
		map[string]string{"email": "close-bg@example.com"}, nil); response.status != http.StatusOK {
		t.Fatalf("找回密码应当 200: %d %v", response.status, response.body)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatalf("后台邮件投递未开始")
	}

	done := make(chan error, 1)
	go func() { done <- app.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("邮件投递与回收站清理都启动时 Close 必须一次取消后及时返回")
	}

	// 共享 context 必须已取消，且 Close 可重复调用。
	if app.backgroundCtx == nil || app.backgroundCtx.Err() == nil {
		t.Fatalf("Close 必须取消共享的 backgroundCtx")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("重复 Close 应当安全: %v", err)
	}
}
