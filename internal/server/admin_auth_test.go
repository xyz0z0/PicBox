package server

// 管理端路由的共同鉴权边界：
//   - 未登录 → 401；
//   - 已登录普通用户 → 403 FORBIDDEN；
//   - Bearer API Key（即使是管理员的 Key）→ 401，管理员接口只接受 Cookie 会话；
//   - 管理员 mustChangePassword=true 时仍可访问（原版 requireAdminUser 只校验角色，
//     强制改密由 UI 的整页改密承担）。

import (
	"net/http"
	"testing"
)

type adminTestRoute struct {
	method string
	path   string
}

var adminTestRoutes = []adminTestRoute{
	{http.MethodGet, "/api/admin/stats"},
	{http.MethodGet, "/api/admin/users"},
	{http.MethodPost, "/api/admin/users"},
	{http.MethodPatch, "/api/admin/users/1"},
	{http.MethodGet, "/api/admin/activation-codes"},
	{http.MethodPost, "/api/admin/activation-codes"},
	{http.MethodPatch, "/api/admin/activation-codes/00000000-0000-0000-0000-000000000000"},
	{http.MethodGet, "/api/admin/images"},
	{http.MethodPatch, "/api/admin/images/00000000-0000-0000-0000-000000000000"},
	{http.MethodDelete, "/api/admin/images/00000000-0000-0000-0000-000000000000"},
	{http.MethodGet, "/api/admin/audit-logs"},
}

func TestAdminRoutesRequireAdminCookieSession(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	_, userCookie := env.addUser(t, "plain@example.com")
	adminID := int64(1)
	adminKey := env.addAPIKey(t, adminID)

	for _, route := range adminTestRoutes {
		route := route
		t.Run(route.method+" "+route.path+"/anonymous", func(t *testing.T) {
			recorder := env.do(t, route.method, route.path, nil, adminTestAuth{})
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("匿名访问应为 401，实际 %d: %s", recorder.Code, recorder.Body.String())
			}
			if code := adminTestErrorCode(t, recorder); code != "UNAUTHORIZED" {
				t.Fatalf("匿名错误码应为 UNAUTHORIZED，实际 %q", code)
			}
		})

		t.Run(route.method+" "+route.path+"/normal-user", func(t *testing.T) {
			recorder := env.do(t, route.method, route.path, nil, adminTestCookieAuth(userCookie))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("普通用户应为 403，实际 %d: %s", recorder.Code, recorder.Body.String())
			}
			if code := adminTestErrorCode(t, recorder); code != "FORBIDDEN" {
				t.Fatalf("普通用户错误码应为 FORBIDDEN，实际 %q", code)
			}
		})

		t.Run(route.method+" "+route.path+"/api-key", func(t *testing.T) {
			recorder := env.do(t, route.method, route.path, nil, adminTestAuth{bearer: adminKey})
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("API Key 访问管理端应为 401，实际 %d: %s", recorder.Code, recorder.Body.String())
			}
		})

		t.Run(route.method+" "+route.path+"/admin", func(t *testing.T) {
			recorder := env.do(t, route.method, route.path, nil, adminTestCookieAuth(adminCookie))
			// 参数/请求体非法的 4xx 也可以接受：这里只断言"不再是鉴权失败"。
			if recorder.Code == http.StatusUnauthorized || recorder.Code == http.StatusForbidden {
				t.Fatalf("管理员不应被鉴权拒绝: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestAdminRoutesAllowMustChangePasswordAdmin(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	if _, err := env.app.db.Exec(`UPDATE "User" SET mustChangePassword = 1 WHERE id = 1`); err != nil {
		t.Fatalf("设置 mustChangePassword 失败: %v", err)
	}

	recorder := env.do(t, http.MethodGet, "/api/admin/stats", nil, adminTestCookieAuth(adminCookie))
	if recorder.Code != http.StatusOK {
		t.Fatalf("强制改密的管理员访问 stats 应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 普通用户即使 mustChangePassword=true 也不是管理员，仍然 403。
	userID, userCookie := env.addUser(t, "plain@example.com")
	if _, err := env.app.db.Exec(`UPDATE "User" SET mustChangePassword = 1 WHERE id = ?`, userID); err != nil {
		t.Fatalf("设置普通用户 mustChangePassword 失败: %v", err)
	}
	recorder = env.do(t, http.MethodGet, "/api/admin/stats", nil, adminTestCookieAuth(userCookie))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应为 403，实际 %d", recorder.Code)
	}
}

func TestAdminUnknownPathStaysNotFound(t *testing.T) {
	env := adminTestNewApp(t, Config{})
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")
	for _, path := range []string{"/api/admin/unknown", "/api/admin/", "/api/admin/users/1/extra"} {
		recorder := env.do(t, http.MethodGet, path, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 应返回 404，实际 %d: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestAdminRoutesMountedByNew 锁定共享契约：core 在 app.go 的 New 中必须挂载
// registerAdminRoutes()。这里故意不调用注册函数，直接通过 New 得到的 Handler 访问，
// 证明部署后的单二进制确实对外提供管理端接口（而不是只靠测试手动注册）。
func TestAdminRoutesMountedByNew(t *testing.T) {
	app, err := New(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() 失败: %v", err)
		}
	})
	env := &adminTestEnv{app: app, handler: app.Handler()}
	adminCookie := env.registerAdmin(t, "root@example.com", "rootpass123")

	for _, target := range []string{"/api/admin/stats", "/api/admin/users", "/api/admin/activation-codes", "/api/admin/images", "/api/admin/audit-logs"} {
		recorder := env.do(t, http.MethodGet, target, nil, adminTestCookieAuth(adminCookie))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 应由 New 挂载的管理端路由直接可用，实际 %d: %s", target, recorder.Code, recorder.Body.String())
		}
	}
}
