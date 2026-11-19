package server

// 管理端全部 11 个路由的注册入口与共享小工具。
//
// 契约见原版 app/api/admin/*（UI：app/admin/AdminClient.tsx、ActivationCodesTab.tsx）：
//
//	GET    /api/admin/stats
//	GET    /api/admin/users
//	POST   /api/admin/users
//	PATCH  /api/admin/users/{id}
//	GET    /api/admin/activation-codes
//	POST   /api/admin/activation-codes
//	PATCH  /api/admin/activation-codes/{id}
//	GET    /api/admin/images
//	PATCH  /api/admin/images/{id}
//	DELETE /api/admin/images/{id}
//	GET    /api/admin/audit-logs
//
// 共同约束（与原版一致）：只接受登录 Cookie 的管理员会话（requireAdminSession，
// API Key 一律拒绝）；业务写入与对应审计行在同一个事务内提交，审计写失败则整体回滚。
// 本文件的实现细节分在 admin_users.go / admin_activation_codes.go / admin_images.go /
// admin_stats.go / admin_audit.go。

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	adminUsersDefaultLimit  = 20
	adminImagesDefaultLimit = 50
	adminAuditDefaultLimit  = 50
	// adminListMaxLimit 与原版 MAX_LIMIT 一致：所有分页列表上限 100。
	adminListMaxLimit = 100
	// adminMaxQueryLength 是用户/激活码列表的搜索词长度上限（原版 64）。
	adminMaxQueryLength = 64
	// adminMaxSearchLength 用于原版没有显式截断的图片/审计搜索，仍然保持有界。
	adminMaxSearchLength = 128
)

// adminRegisteredRoutes 记录已经注册过管理端路由的 App。
// registerAdminRoutes 由 core 在 New 中调用，测试也会显式调用一次；ServeMux 对同一
// 模式重复注册会 panic，因此这里做成幂等，注册顺序与调用次数都不影响结果。
var adminRegisteredRoutes sync.Map

func (a *App) registerAdminRoutes() {
	if _, loaded := adminRegisteredRoutes.LoadOrStore(a, struct{}{}); loaded {
		return
	}
	a.mux.HandleFunc("GET /api/admin/stats", a.adminHandleStats)

	a.mux.HandleFunc("GET /api/admin/users", a.adminHandleUsersList)
	a.mux.HandleFunc("POST /api/admin/users", a.adminHandleUsersCreate)
	a.mux.HandleFunc("PATCH /api/admin/users/{id}", a.adminHandleUserUpdate)

	a.mux.HandleFunc("GET /api/admin/activation-codes", a.adminHandleActivationCodesList)
	a.mux.HandleFunc("POST /api/admin/activation-codes", a.adminHandleActivationCodesCreate)
	a.mux.HandleFunc("PATCH /api/admin/activation-codes/{id}", a.adminHandleActivationCodeRevoke)

	a.mux.HandleFunc("GET /api/admin/images", a.adminHandleImagesList)
	a.mux.HandleFunc("PATCH /api/admin/images/{id}", a.adminHandleImageUpdate)
	a.mux.HandleFunc("DELETE /api/admin/images/{id}", a.adminHandleImageDelete)

	a.mux.HandleFunc("GET /api/admin/audit-logs", a.adminHandleAuditLogs)
}

// ---- 请求体 ----

// adminReadJSONObject 读取有界 JSON 对象，失败时按调用方给出的错误码响应。
// 原版 /api/admin/users 与 /api/admin/activation-codes 的 POST 都用 INVALID_BODY。
func adminReadJSONObject(w http.ResponseWriter, r *http.Request, code, message string) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, imagesMaxRequestJSON+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, code, message)
		return nil, false
	}
	if int64(len(body)) > imagesMaxRequestJSON {
		writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "请求体超过大小限制")
		return nil, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		writeError(w, http.StatusBadRequest, code, message)
		return nil, false
	}
	return payload, true
}

// adminLooseJSONObject 用于 PATCH 一类"请求体非法时等价于缺少字段"的接口：
// 原版 PATCH 用 req.json().catch(() => null)，因此坏 JSON 会落到缺 operation 的分支，
// 返回 INVALID_OPERATION / INVALID_BODY 而不是 JSON 解析错误。
func adminLooseJSONObject(r *http.Request) map[string]json.RawMessage {
	body, err := io.ReadAll(io.LimitReader(r.Body, imagesMaxRequestJSON+1))
	if err != nil || int64(len(body)) > imagesMaxRequestJSON {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload
}

// adminStringField 读取字符串字段；缺失、null 或非字符串都按"没有提供"处理。
func adminStringField(body map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := body[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// adminParseJSONInt 只接受 JSON 整数（含 1e3 / 1.0 这类整数值写法）。
// 小数、负数之外的超范围值、字符串、布尔与 null 都返回 false。
func adminParseJSONInt(raw []byte) (int64, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" {
		return 0, false
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		return parsed, true
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	// 超出 2^53 的浮点已无法保证整数精度，宁可拒绝也不静默换算成错误配额。
	if parsed != math.Trunc(parsed) || math.Abs(parsed) > 1<<53 {
		return 0, false
	}
	return int64(parsed), true
}

// adminIntField 读取整数字段（如 count / expiresInDays）。
func adminIntField(body map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := body[key]
	if !ok {
		return 0, false
	}
	return adminParseJSONInt(raw)
}

// ---- 分页与游标 ----

type adminListPaging struct {
	Page  int
	Limit int
	Query string
}

// adminParsePaging 复刻原版分页参数：limit 默认值随接口、夹到 1..100；
// page 非法或小于 1 时按 1 处理（真正的有效页夹紧在算出 total 之后）。
func adminParsePaging(query url.Values, defaultLimit, maxQueryLength int) adminListPaging {
	paging := adminListPaging{Page: 1, Limit: defaultLimit}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			paging.Limit = parsed
		}
	}
	if paging.Limit < 1 {
		paging.Limit = 1
	}
	if paging.Limit > adminListMaxLimit {
		paging.Limit = adminListMaxLimit
	}
	if raw := strings.TrimSpace(query.Get("page")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			paging.Page = parsed
		}
	}
	paging.Query = adminTruncateQuery(query.Get("q"), maxQueryLength)
	return paging
}

// adminTruncateQuery 去掉首尾空白并按字符（不是字节）截断，避免把多字节字符切成半个。
func adminTruncateQuery(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// adminClampPage 把请求页夹到 [1, totalPages]，并返回 totalPages（至少 1）。
func adminClampPage(page int, total int64, limit int) (int, int) {
	if limit < 1 {
		limit = 1
	}
	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	return page, totalPages
}

// adminEncodeCursor 生成同时包含 (createdAt, id) 的 opaque 游标：
// 只按 createdAt 翻页时，同一时间戳的多条记录会在页边界重复或丢失，带上 id 才能稳定。
func adminEncodeCursor(createdAt, id string) string {
	return createdAt + "|" + id
}

// adminParseCursor 解析游标，返回参与比较的时间戳原文与可选的 id。
//
// 时间戳原文直接用于 TEXT 比较（不做格式重写）：调用方返回给 UI 的游标就是库里存的值，
// 重新格式化会让"同一秒"的记录在 < / = 比较中错位。
// 无法解析的游标按"没有游标"处理（与原版忽略非法 cursor 的行为一致）。
func adminParseCursor(value string) (createdAt string, id string, ok bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", "", false
	}
	createdAt = trimmed
	if index := strings.Index(trimmed, "|"); index > 0 {
		createdAt = trimmed[:index]
		id = trimmed[index+1:]
	}
	if _, parsed := imagesParseTime(createdAt); !parsed {
		return "", "", false
	}
	return createdAt, id, true
}

// adminCursorIDInt 把游标里的 id 解析为审计日志等整数主键；非法值当作没有 id。
func adminCursorIDInt(id string) (int64, bool) {
	if id == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// ---- 审计动作白名单（供 audit-logs 的 action 参数校验） ----

func adminAuditActionParam(query url.Values) (string, bool) {
	action := strings.TrimSpace(query.Get("action"))
	if action == "" || action == "all" {
		return "all", true
	}
	if !auditActionAllowed(action) {
		return "", false
	}
	return action, true
}
