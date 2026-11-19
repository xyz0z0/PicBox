package server

import (
	"net/http"

	"github.com/google/uuid"
)

// requireAdminSession 只接受登录 Cookie 的管理员会话，返回当前管理员账号。
//
// 与原版 /api/admin/* 的共同约束一致：
//   - Bearer API Key 一律拒绝（管理员接口不开放给 Key）；
//   - 未登录 401 UNAUTHORIZED，账号被禁用 403 ACCOUNT_DISABLED；
//   - 已登录但不是管理员 403 FORBIDDEN。
//
// 返回 false 时响应已写好，调用方直接 return 即可。
func (a *App) requireAdminSession(w http.ResponseWriter, r *http.Request) (*User, bool) {
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return nil, false
	}
	if user.Role != roleAdmin {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "需要管理员权限")
		return nil, false
	}
	return user, true
}

const roleAdmin = "ADMIN"

const roleUser = "USER"

// newInternalUsername 生成与原版一致的内部用户名（user_<uuid>），
// 不暴露给登录：登录标识只有邮箱。
func newInternalUsername() string {
	return "user_" + uuid.NewString()
}
