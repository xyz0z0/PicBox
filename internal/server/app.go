package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// App 是 Go 预览服务的运行时状态。db/mux/uploadSlots/secret 是各 worker 共享的
// 契约字段：images worker 使用 a.db 与 a.uploadSlots，web worker 向 a.mux 注册页面路由。
type App struct {
	db          *sql.DB
	cfg         Config
	mux         *http.ServeMux
	uploadSlots chan struct{}
	secret      []byte

	registerLimiter       *rateLimiter
	loginLimiter          *rateLimiter
	loginEmailLimiter     *rateLimiter
	passwordLimiter       *rateLimiter
	passwordUserLimiter   *rateLimiter
	recoverySendLimiter   *rateLimiter
	recoveryMailLimiter   *rateLimiter
	recoveryVerifyLimiter *rateLimiter
	// uploadLimiter 按客户端 IP 限制图片上传 / URL 导入频率（原版 100 次/小时）。
	// 它只限制总量，不代替 uploadSlots 的并发背压。
	uploadLimiter *rateLimiter
	bcryptSlots   chan struct{}

	// mailJobs/mailSend 是找回密码、修改邮箱的投递通道：有界队列 + 固定 worker；
	// mailSend 默认指向 sendSMTPMail，测试可注入。
	mailJobs chan mailJob
	mailSend mailSender

	// backgroundCtx/backgroundCancel/backgroundWG 是所有后台任务（回收站保留期清理、
	// 账号邮件投递）共享的唯一生命周期入口：New 只创建一次，startTrashRetention 与
	// 邮件 worker 都复用它；Close 只 cancel 一次，再等待后台任务退出并关闭数据库。
	backgroundCtx    context.Context
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// New 归一化配置、准备数据目录与密钥、打开数据库并注册路由。
// Config{DataDir: t.TempDir()} 即可得到一个可用的测试实例。
func New(cfg Config) (*App, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	// 环境变量里的 JWT_SECRET 非法时必须在创建任何文件之前失败。
	if err := validateJWTSecretEnv(); err != nil {
		return nil, err
	}
	if err := ensureDataDir(normalized.DataDir); err != nil {
		return nil, err
	}
	// 先校验并打开数据库：外部数据库会在创建密钥/暂存目录之前被拒绝，
	// 失败时不在数据目录留下任何本服务的文件。
	db, err := openDatabase(normalized.DataDir)
	if err != nil {
		return nil, err
	}
	secret, err := loadOrCreateSecret(normalized.DataDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	app := &App{
		db:                    db,
		cfg:                   normalized,
		mux:                   http.NewServeMux(),
		uploadSlots:           make(chan struct{}, 1),
		secret:                secret,
		registerLimiter:       newRateLimiter(10, 10*time.Minute, defaultRateLimitCapacity),
		loginLimiter:          newRateLimiter(20, 5*time.Minute, defaultRateLimitCapacity),
		loginEmailLimiter:     newRateLimiter(10, 5*time.Minute, defaultRateLimitCapacity),
		passwordLimiter:       newRateLimiter(passwordChangeIPLimit, passwordChangeWindow, defaultRateLimitCapacity),
		passwordUserLimiter:   newRateLimiter(passwordChangeUserLimit, passwordChangeWindow, defaultRateLimitCapacity),
		recoverySendLimiter:   newRateLimiter(10, 10*time.Minute, defaultRateLimitCapacity),
		recoveryMailLimiter:   newRateLimiter(3, 10*time.Minute, defaultRateLimitCapacity),
		recoveryVerifyLimiter: newRateLimiter(30, 10*time.Minute, defaultRateLimitCapacity),
		uploadLimiter:         newRateLimiter(uploadImageRateLimit, uploadImageRateWindow, defaultRateLimitCapacity),
		bcryptSlots:           make(chan struct{}, 4),
		mailJobs:              make(chan mailJob, mailQueueCapacity),
	}
	app.mailSend = app.sendSMTPMail

	// 后台任务（回收站保留期清理、账号邮件投递）共用下面这个 context：
	// Close 只 cancel 一次再等待，保证不会在数据库关闭后还有任务在写。
	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	app.backgroundCtx = backgroundCtx
	app.backgroundCancel = backgroundCancel

	// 三组路由分别由 core / images / web 文件实现，按共享契约在 New 中注册。
	// 注册路由之前先修复崩溃遗留的回收站清理：残留字节要么搬回原路径，要么在数据库行
	// 已删除时清理掉；失败则启动失败并保留现场，不静默丢文件。
	if err := app.imagesRecoverInterruptedPurges(backgroundCtx); err != nil {
		backgroundCancel()
		_ = db.Close()
		return nil, err
	}
	app.startMailWorkers(backgroundCtx)
	// 回收站保留期清理（另一组 worker 提供）在启动恢复完成后开始。
	app.startTrashRetention()

	app.registerAuthRoutes()
	app.registerImageRoutes()
	// 图片工作台的补全路由（tags/usage/batch/from-url/export/meta）由 images worker 提供。
	app.registerImagesExtraRoutes()
	// 管理端路由（stats/users/activation-codes/images/audit-logs）由 admin worker 提供。
	app.registerAdminRoutes()
	app.registerWebRoutes()
	return app, nil
}

// Handler 返回带通用中间件的 HTTP 处理器。
func (a *App) Handler() http.Handler {
	return a.recoverPanics(a.enforceSameOriginWrites(a.commonHeaders(a.webUnknownNavigation(a.mux))))
}

// Close 先取消并等待后台任务（回收站清理、邮件投递）结束，再关闭数据库，可重复调用。
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if a.backgroundCancel != nil {
			a.backgroundCancel()
		}
		a.backgroundWG.Wait()
		if a.db != nil {
			a.closeErr = a.db.Close()
		}
	})
	return a.closeErr
}

func loadOrCreateSecret(dataDir string) ([]byte, error) {
	if err := validateJWTSecretEnv(); err != nil {
		return nil, err
	}
	if secret := os.Getenv("JWT_SECRET"); secret != "" {
		return []byte(secret), nil
	}

	keyPath := filepath.Join(dataDir, authKeyFileName)
	raw, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decodeErr != nil || len(decoded) < minJWTSecretBytes {
			return nil, fmt.Errorf("%s 内容非法，拒绝用未知密钥启动", keyPath)
		}
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return nil, fmt.Errorf("设置 %s 权限失败: %w", keyPath, err)
		}
		return decoded, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("读取 %s 失败: %w", keyPath, err)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("生成 JWT 密钥失败: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(secret)

	tmp, err := os.CreateTemp(dataDir, ".auth-key-*")
	if err != nil {
		return nil, fmt.Errorf("创建 JWT 密钥文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("设置 JWT 密钥文件权限失败: %w", err)
	}
	if _, err := tmp.WriteString(encoded); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("写入 JWT 密钥失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("写入 JWT 密钥失败: %w", err)
	}
	if err := os.Rename(tmpPath, keyPath); err != nil {
		return nil, fmt.Errorf("保存 JWT 密钥失败: %w", err)
	}
	return secret, nil
}

func (a *App) commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *App) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("请求处理 panic: %s %s: %v", r.Method, r.URL.Path, recovered)
				if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
					writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// enforceSameOriginWrites 拒绝跨源写请求；没有 Origin 的 CLI 请求允许。
// 配置了 BASEURL 时只信任它；否则只允许来自环回地址且与请求 Host 一致的来源，
// 不因为反向代理提供的 Host 头而放行任意外部来源。
func (a *App) enforceSameOriginWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) {
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
				if !a.originAllowed(origin, r) {
					w.Header().Set("Vary", "Origin")
					writeError(w, http.StatusForbidden, "CROSS_ORIGIN_DENIED", "跨源写请求被拒绝")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func (a *App) originAllowed(origin string, r *http.Request) bool {
	if origin == "null" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	originKey := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	if allowed, ok := baseURLOrigin(a.cfg.BaseURL); ok {
		return originKey == allowed
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (a *App) handleUnknownAPI(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", "接口不存在或尚未实现")
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	var one int
	if err := a.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		writeError(w, http.StatusServiceUnavailable, "DB_UNAVAILABLE", "数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("写入 JSON 响应失败: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok": false,
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
