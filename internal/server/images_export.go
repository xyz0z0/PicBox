package server

// GET|HEAD /api/images/export：把账号可导出的原图流式打包成 ZIP。
//
// 关键契约（与原版 app/api/images/export + lib/imageExport + lib/exportCooldown 对齐）：
//   - HEAD 只做鉴权、范围校验与预检，返回同样的成功头，但从不声明预约、不消耗冷却；
//   - GET 在确认可以流式导出之后才原子声明「进行中预约」，并发 GET 只有一个赢家；
//   - 只有服务端成功写完 ZIP 才写成功冷却时间；失败/取消立即释放预约，可立即重试；
//   - 预约带 10 分钟租约，每 3 分钟按 token 条件续租，崩溃后租约到期可被接管；
//   - 归档按文件惰性读取，单张原图读完即关闭，不把图片字节全量读进内存。

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	imagesExportMaxSelected           = 100
	imagesExportMaxAllImages          = 10000
	imagesExportMaxTotalBytes   int64 = 2 * 1024 * 1024 * 1024 // 2 GiB
	imagesExportReadmeName            = "README.txt"
	imagesExportManifestName          = "manifest.json"
	imagesExportVersion               = 1
	imagesExportDefaultCooldown       = 12
)

// 预约租约与续租间隔。测试会临时覆盖这两个变量，生产路径始终使用默认值。
var (
	imagesExportLease         = 10 * time.Minute
	imagesExportRenewInterval = 3 * time.Minute
)

// ---- 数据模型 ----

type imagesExportScope struct {
	mode string // "all" | "ids"
	ids  []string
}

type imagesExportImage struct {
	ID               string
	OriginalName     string
	OriginalMimeType string
	OriginalSize     int64
	Width            int64
	Height           int64
	CreatedAt        string
	UpdatedAt        string
	IsVisible        bool
	OriginalPath     string
	Tags             []string
}

type imagesExportManifestEntry struct {
	ID               string   `json:"id"`
	OriginalName     string   `json:"originalName"`
	Tags             []string `json:"tags"`
	CreatedAt        string   `json:"createdAt"`
	UpdatedAt        string   `json:"updatedAt"`
	IsVisible        bool     `json:"isVisible"`
	OriginalMimeType string   `json:"originalMimeType"`
	OriginalSize     int64    `json:"originalSize"`
	ContentType      string   `json:"contentType"`
	Size             int64    `json:"size"`
	Width            int64    `json:"width"`
	Height           int64    `json:"height"`
	ArchivePath      string   `json:"archivePath"`
}

type imagesExportManifest struct {
	Version     int                         `json:"version"`
	GeneratedAt string                      `json:"generatedAt"`
	Scope       string                      `json:"scope"`
	Count       int                         `json:"count"`
	TotalBytes  int64                       `json:"totalBytes"`
	Format      string                      `json:"format"`
	Images      []imagesExportManifestEntry `json:"images"`
}

// ---- 范围解析 ----

// imagesExportIDPattern 只接受 UUID 形式的图片 id（当前所有图片均由 uuid.NewString() 生成）。
var imagesExportIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// imagesExportIDValid 同时要求大小写归一后的长度，避免接受 uuid.Parse 的宽松形态。
func imagesExportIDValid(id string) bool {
	return len(id) == 36 && imagesExportIDPattern.MatchString(strings.ToLower(id))
}

// imagesParseExportScope 复刻原版 parseExportScope 的参数与错误码。
func imagesParseExportScope(query url.Values) (imagesExportScope, error) {
	rawScope, hasScope := query["scope"]
	rawIDs := query["ids"]
	hasIDs := len(rawIDs) > 0

	scopeValue := ""
	if hasScope {
		if len(rawScope) != 1 {
			return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_SCOPE", message: "scope 只能是 all 或 ids"}
		}
		scopeValue = strings.TrimSpace(rawScope[0])
		if scopeValue != "all" && scopeValue != "ids" {
			return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_SCOPE", message: "scope 只能是 all 或 ids"}
		}
	}
	if scopeValue == "all" && hasIDs {
		return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_SCOPE", message: "scope=all 不能与 ids 同时使用"}
	}
	if scopeValue == "ids" && !hasIDs {
		return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_IDS", message: "scope=ids 时必须提供至少一个 ids"}
	}
	if !hasScope && !hasIDs {
		return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_SCOPE", message: "请提供 scope=all 或重复的 ids 参数"}
	}
	if !hasIDs {
		return imagesExportScope{mode: "all"}, nil
	}

	seen := make(map[string]struct{}, len(rawIDs))
	ids := make([]string, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := strings.TrimSpace(raw)
		if !imagesExportIDValid(id) {
			return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "INVALID_IDS", message: "ids 必须是合法的图片 UUID"}
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return imagesExportScope{}, &imagesError{status: http.StatusBadRequest, code: "EMPTY_SELECTION", message: "请至少选择一张图片再导出"}
	}
	if len(ids) > imagesExportMaxSelected {
		return imagesExportScope{}, &imagesError{
			status:  http.StatusBadRequest,
			code:    "TOO_MANY_SELECTED",
			message: fmt.Sprintf("单次最多选择 %d 张图片导出", imagesExportMaxSelected),
		}
	}
	return imagesExportScope{mode: "ids", ids: ids}, nil
}

// ---- 数据加载与预检 ----

const imagesExportColumns = `id, originalName, originalMimeType, size, width, height, createdAt, updatedAt, isVisible, originalPath`

// imagesLoadExportImages 载入可导出图片。范围始终限定 uploaderId + 未软删 + 非管理员
// 下架；隐藏图片仍导出。显式选择里任何一个 id 越权/不存在/已删除都整体 403。
func (a *App) imagesLoadExportImages(ctx context.Context, userID int64, scope imagesExportScope) ([]imagesExportImage, error) {
	base := `uploaderId = ? AND deletedAt IS NULL AND deletedByAdmin = 0`
	var (
		rows *sql.Rows
		err  error
	)
	if scope.mode == "all" {
		rows, err = a.db.QueryContext(ctx,
			`SELECT `+imagesExportColumns+` FROM "Image" WHERE `+base+` ORDER BY createdAt DESC, id DESC LIMIT ?`,
			userID, imagesExportMaxAllImages+1)
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(scope.ids)), ",")
		args := make([]any, 0, len(scope.ids)+1)
		args = append(args, userID)
		for _, id := range scope.ids {
			args = append(args, id)
		}
		rows, err = a.db.QueryContext(ctx,
			`SELECT `+imagesExportColumns+` FROM "Image" WHERE `+base+` AND id IN (`+placeholders+`)`, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("read export images: %w", err)
	}

	loaded := make([]imagesExportImage, 0, 64)
	for rows.Next() {
		var (
			image      imagesExportImage
			isVisible  int
			width      sql.NullInt64
			height     sql.NullInt64
			originalPN string
		)
		if err := rows.Scan(&image.ID, &image.OriginalName, &image.OriginalMimeType, &image.OriginalSize,
			&width, &height, &image.CreatedAt, &image.UpdatedAt, &isVisible, &originalPN); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan export image: %w", err)
		}
		image.Width = width.Int64
		image.Height = height.Int64
		image.IsVisible = isVisible != 0
		image.OriginalPath = originalPN
		image.Tags = []string{}
		loaded = append(loaded, image)
	}
	scanErr := rows.Err()
	// 单连接驱动：读取标签前必须关闭结果集。
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("read export images: %w", scanErr)
	}

	if scope.mode == "all" {
		if len(loaded) > imagesExportMaxAllImages {
			return nil, &imagesError{
				status:  http.StatusBadRequest,
				code:    "TOO_MANY_IMAGES",
				message: fmt.Sprintf("一次最多导出 %d 张图片，请分批导出", imagesExportMaxAllImages),
			}
		}
		if len(loaded) == 0 {
			return nil, &imagesError{status: http.StatusNotFound, code: "NO_IMAGES", message: "当前账号没有可导出的图片"}
		}
	} else if len(loaded) != len(scope.ids) {
		return nil, &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "部分图片不存在、已删除或不属于当前账号，已取消本次导出"}
	}

	ids := make([]string, 0, len(loaded))
	for _, image := range loaded {
		ids = append(ids, image.ID)
	}
	tagsByImage, err := imagesLoadTagsFor(ctx, a.db, ids)
	if err != nil {
		return nil, err
	}
	for index := range loaded {
		if tags := tagsByImage[loaded[index].ID]; tags != nil {
			loaded[index].Tags = tags
		}
	}

	if scope.mode == "ids" {
		// 显式选择保持调用者给出的顺序，manifest 与归档顺序因此可预测。
		byID := make(map[string]imagesExportImage, len(loaded))
		for _, image := range loaded {
			byID[image.ID] = image
		}
		ordered := make([]imagesExportImage, 0, len(scope.ids))
		for _, id := range scope.ids {
			ordered = append(ordered, byID[id])
		}
		return ordered, nil
	}
	return loaded, nil
}

// imagesExportArchiveTarget 只信任持久化存储路径的扩展名，不猜测未知格式。
func imagesExportArchiveTarget(image imagesExportImage) (archivePath, contentType string, err error) {
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(image.OriginalPath), "."))
	switch extension {
	case "webp", "png", "jpg", "jpeg", "gif":
	default:
		return "", "", fmt.Errorf("不支持的存储图片格式 %q（图片 %s）", extension, image.ID)
	}
	return "images/" + image.ID + "." + extension,
		imagesContentType(image.OriginalPath, image.OriginalMimeType, imagesVariantOriginal), nil
}

// imagesExportPreflight 只做 stat：确认每个原图存在、统计真实字节数、提前拒绝未知格式。
// 必须在写响应头（以及声明预约）之前完成，保证缺失文件/超限返回明确状态码。
func (a *App) imagesExportPreflight(images []imagesExportImage) (map[string]int64, int64, error) {
	sizes := make(map[string]int64, len(images))
	var total int64
	for _, image := range images {
		if _, _, err := imagesExportArchiveTarget(image); err != nil {
			return nil, 0, &imagesError{status: http.StatusInternalServerError, code: "EXPORT_FAILED", message: "存在无法归档的图片格式，已取消本次导出"}
		}
		absolute, err := a.imagesSafePath(image.OriginalPath)
		if err != nil {
			return nil, 0, &imagesError{status: http.StatusNotFound, code: "EXPORT_FILE_MISSING", message: "部分图片文件缺失，已取消本次导出"}
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.Mode().IsRegular() {
			return nil, 0, &imagesError{status: http.StatusNotFound, code: "EXPORT_FILE_MISSING", message: "部分图片文件缺失，已取消本次导出"}
		}
		sizes[image.ID] = info.Size()
		total += info.Size()
		if total > imagesExportMaxTotalBytes {
			return nil, 0, &imagesError{status: http.StatusRequestEntityTooLarge, code: "EXPORT_TOO_LARGE", message: "导出总大小超过 2 GiB 限制，请分批导出"}
		}
	}
	return sizes, total, nil
}

// ---- 冷却与预约 ----

type imagesExportState struct {
	lastExportAt   sql.NullString
	reservationAt  sql.NullString
	reservationTok sql.NullString
}

type imagesExportLimit struct {
	limited           bool
	reason            string // "cooldown" | "in_progress"
	retryAfterSeconds int
}

func (a *App) imagesExportCooldownHours() int {
	if a.cfg.ExportCooldownHours < 0 {
		return imagesExportDefaultCooldown
	}
	return a.cfg.ExportCooldownHours
}

// imagesExportTimestamp 把任意时刻写成服务统一的 UTC 时间字面量。
//
// imagesTimeLayout 里的 Z 是字面字符，不做时区换算，所以任何"本地时区"的 now 直接
// Format 都会写出一个偏移了 δ 的字符串：δ>0 时成功冷却变成 12h+δ、崩溃遗留的租约变成
// 10min+δ；δ<0 时租约一写入就已过期（单飞失效）。库里的其它时间戳（imagesNow、
// audit coreTimestamp）都是 UTC，这里必须保持一致。
func imagesExportTimestamp(at time.Time) string {
	return at.UTC().Truncate(time.Millisecond).Format(imagesTimeLayout)
}

func (a *App) imagesExportReadState(ctx context.Context, userID int64) (imagesExportState, error) {
	var state imagesExportState
	err := a.db.QueryRowContext(ctx,
		`SELECT lastExportAt, exportReservationAt, exportReservationToken FROM "User" WHERE id = ?`, userID).
		Scan(&state.lastExportAt, &state.reservationAt, &state.reservationTok)
	if errors.Is(err, sql.ErrNoRows) {
		return state, &imagesError{status: http.StatusUnauthorized, code: "UNAUTHORIZED", message: "账号不存在"}
	}
	if err != nil {
		return state, fmt.Errorf("read export reservation: %w", err)
	}
	return state, nil
}

// imagesExportLimitFor 只读判定：成功冷却（关闭冷却时不参与）与进行中租约取更晚到期者。
func (a *App) imagesExportLimitFor(state imagesExportState, now time.Time) imagesExportLimit {
	var (
		until  time.Time
		reason string
	)
	hours := a.imagesExportCooldownHours()
	if hours > 0 && state.lastExportAt.Valid {
		if at, ok := imagesParseTime(state.lastExportAt.String); ok {
			candidate := at.Add(time.Duration(hours) * time.Hour)
			if candidate.After(now) {
				until, reason = candidate, "cooldown"
			}
		} else {
			log.Printf("images: 无法解析 lastExportAt=%q，按未冷却处理", state.lastExportAt.String)
		}
	}
	if state.reservationAt.Valid {
		if at, ok := imagesParseTime(state.reservationAt.String); ok {
			candidate := at.Add(imagesExportLease)
			if candidate.After(now) && (reason == "" || candidate.After(until)) {
				until, reason = candidate, "in_progress"
			}
		} else {
			log.Printf("images: 无法解析 exportReservationAt=%q，按未预约处理", state.reservationAt.String)
		}
	}
	if reason == "" {
		return imagesExportLimit{}
	}
	seconds := int(math.Ceil(until.Sub(now).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return imagesExportLimit{limited: true, reason: reason, retryAfterSeconds: seconds}
}

// imagesExportClaim 用一条条件 UPDATE 原子声明预约。条件里带上读取到的旧值做 CAS，
// 因此并发 GET 只有一个能命中一行；其余请求拿到受限状态而不是排队。
func (a *App) imagesExportClaim(ctx context.Context, userID int64, state imagesExportState, now time.Time) (string, bool, error) {
	token := uuid.NewString()
	query := `UPDATE "User" SET exportReservationAt = ?, exportReservationToken = ? WHERE id = ?`
	args := []any{imagesExportTimestamp(now), token, userID}
	if state.reservationAt.Valid {
		query += ` AND exportReservationAt = ?`
		args = append(args, state.reservationAt.String)
	} else {
		query += ` AND exportReservationAt IS NULL`
	}
	if state.lastExportAt.Valid {
		query += ` AND lastExportAt = ?`
		args = append(args, state.lastExportAt.String)
	} else {
		query += ` AND lastExportAt IS NULL`
	}
	result, err := a.db.ExecContext(ctx, query, args...)
	if err != nil {
		return "", false, fmt.Errorf("claim export reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("count claimed reservations: %w", err)
	}
	if affected != 1 {
		return "", false, nil
	}
	return token, true, nil
}

// imagesExportRelease 只在 token 仍然匹配时清除预约；绝不修改 lastExportAt。
func (a *App) imagesExportRelease(ctx context.Context, userID int64, token string) (bool, error) {
	result, err := a.db.ExecContext(ctx,
		`UPDATE "User" SET exportReservationAt = NULL, exportReservationToken = NULL
		WHERE id = ? AND exportReservationToken = ?`, userID, token)
	if err != nil {
		return false, fmt.Errorf("release export reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count released reservations: %w", err)
	}
	return affected == 1, nil
}

// imagesExportFinalize 是唯一的成功路径：token 仍匹配才写成功时间戳并清预约。
// 过期回调不会误记成功，也不会清掉新的预约。
func (a *App) imagesExportFinalize(ctx context.Context, userID int64, token string, now time.Time) (bool, error) {
	result, err := a.db.ExecContext(ctx,
		`UPDATE "User" SET lastExportAt = ?, exportReservationAt = NULL, exportReservationToken = NULL
		WHERE id = ? AND exportReservationToken = ?`, imagesExportTimestamp(now), userID, token)
	if err != nil {
		return false, fmt.Errorf("finalize export reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count finalized reservations: %w", err)
	}
	return affected == 1, nil
}

// imagesExportRenew 按 token 条件续租，避免长导出在 10 分钟后被接管。
func (a *App) imagesExportRenew(ctx context.Context, userID int64, token string) (bool, error) {
	result, err := a.db.ExecContext(ctx,
		`UPDATE "User" SET exportReservationAt = ? WHERE id = ? AND exportReservationToken = ?`,
		imagesExportTimestamp(time.Now()), userID, token)
	if err != nil {
		return false, fmt.Errorf("renew export reservation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count renewed reservations: %w", err)
	}
	return affected == 1, nil
}

// imagesExportKeepLease 启动续租协程。租约丢失（token 已被接管）时调用 onLost，
// 让调用方中断已经失效的流；返回的 stop 必须在导出结束时调用，避免协程泄漏。
func (a *App) imagesExportKeepLease(ctx context.Context, userID int64, token string, onLost func()) func() {
	leaseCtx, cancelLease := context.WithCancel(ctx)
	stop := make(chan struct{})
	var once sync.Once
	stopLease := func() {
		once.Do(func() {
			close(stop)
			cancelLease()
		})
	}
	go func() {
		ticker := time.NewTicker(imagesExportRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				ok, err := a.imagesExportRenew(renewCtx, userID, token)
				cancel()
				if leaseCtx.Err() != nil || err != nil {
					if err != nil && leaseCtx.Err() == nil {
						log.Printf("images: 导出预约续租失败（账号 %d）: %v", userID, err)
					}
					continue
				}
				if !ok {
					// 主动收尾时 token 已被清空，不算租约丢失。
					log.Printf("images: 导出预约已被接管（账号 %d），中断过期流", userID)
					onLost()
					return
				}
			}
		}
	}()
	return stopLease
}

// ---- HTTP 入口 ----

func (a *App) imagesHandleExport(w http.ResponseWriter, r *http.Request) {
	a.imagesHandleExportRequest(w, r, r.Method == http.MethodHead)
}

func (a *App) imagesHandleExportRequest(w http.ResponseWriter, r *http.Request, isHead bool) {
	ctx := r.Context()
	// 自己解析身份而不是用 authenticate：HEAD 没有 body，401/403 也必须带上
	// X-Export-Error/X-Export-Message，前端 ExportDialog 只读这两组头。
	user, failure := a.resolveUser(r, true)
	if failure != nil {
		a.imagesExportError(w, isHead, failure.status, failure.code, failure.message)
		return
	}
	if user == nil {
		a.imagesExportError(w, isHead, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}

	scope, err := imagesParseExportScope(r.URL.Query())
	if err != nil {
		a.imagesExportFail(w, isHead, err)
		return
	}

	// 只读快查：冷却中或已有导出的请求不必跑 load + preflight。HEAD 也只读。
	state, err := a.imagesExportReadState(ctx, user.ID)
	if err != nil {
		a.imagesExportFail(w, isHead, err)
		return
	}
	if limit := a.imagesExportLimitFor(state, time.Now()); limit.limited {
		a.imagesExportLimited(w, isHead, limit)
		return
	}

	images, err := a.imagesLoadExportImages(ctx, user.ID, scope)
	if err != nil {
		a.imagesExportFail(w, isHead, err)
		return
	}
	sizes, totalBytes, err := a.imagesExportPreflight(images)
	if err != nil {
		a.imagesExportFail(w, isHead, err)
		return
	}

	if isHead {
		a.imagesExportSetSuccessHeaders(w, scope, len(images), totalBytes)
		w.WriteHeader(http.StatusOK)
		return
	}

	token, acquired, err := a.imagesExportClaim(ctx, user.ID, state, time.Now())
	if err != nil {
		a.imagesExportFail(w, isHead, err)
		return
	}
	if !acquired {
		fresh, readErr := a.imagesExportReadState(ctx, user.ID)
		if readErr != nil {
			a.imagesExportFail(w, isHead, readErr)
			return
		}
		limit := a.imagesExportLimitFor(fresh, time.Now())
		if !limit.limited {
			// 账号行不可见等极端竞态：按短暂受限处理，绝不绕过单飞约束。
			limit = imagesExportLimit{limited: true, reason: "in_progress", retryAfterSeconds: 1}
		}
		a.imagesExportLimited(w, isHead, limit)
		return
	}

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stopRenew := a.imagesExportKeepLease(streamCtx, user.ID, token, cancelStream)
	defer stopRenew()

	a.imagesExportSetSuccessHeaders(w, scope, len(images), totalBytes)
	w.WriteHeader(http.StatusOK)
	writeErr := a.imagesExportWriteArchive(streamCtx, w, images, sizes, scope)
	if writeErr != nil || streamCtx.Err() != nil {
		if writeErr != nil {
			log.Printf("images: 导出中断（账号 %d）: %v", user.ID, writeErr)
		}
		// 失败/取消立刻释放预约，用户可以马上重试；不写成功冷却。
		stopRenew()
		a.imagesExportSettle(context.Background(), user.ID, token, false)
		return
	}
	// 先停续租再收尾：避免收尾写入后还在跑的续租把"token 已清空"误报成租约丢失。
	stopRenew()
	a.imagesExportSettle(context.Background(), user.ID, token, true)
}

// imagesExportSettle 收尾预约。使用独立 context：请求 context 在流结束时可能已被取消，
// 但预约必须落库，否则用户会被租约锁住 10 分钟。
func (a *App) imagesExportSettle(ctx context.Context, userID int64, token string, success bool) {
	settleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if success {
		ok, err := a.imagesExportFinalize(settleCtx, userID, token, time.Now())
		if err != nil {
			log.Printf("images: 导出成功但收尾失败（账号 %d）: %v", userID, err)
			return
		}
		if !ok {
			log.Printf("images: 导出成功但预约已被接管（账号 %d），未写入冷却", userID)
		}
		return
	}
	if _, err := a.imagesExportRelease(settleCtx, userID, token); err != nil {
		log.Printf("images: 释放导出预约失败（账号 %d）: %v", userID, err)
	}
}

func (a *App) imagesExportSetSuccessHeaders(w http.ResponseWriter, scope imagesExportScope, count int, totalBytes int64) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+imagesExportArchiveFileName(scope, time.Now())+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Export-Count", strconv.Itoa(count))
	w.Header().Set("X-Export-Bytes", strconv.FormatInt(totalBytes, 10))
	w.Header().Set("X-Export-Cooldown-Hours", strconv.Itoa(a.imagesExportCooldownHours()))
}

// imagesExportError 输出错误响应。HEAD 没有 body，错误码与文案通过 X-Export-* 头传递；
// GET 保留 JSON 封装，但 Cache-Control 必须是 private/no-store（导出状态不可共享缓存）。
func (a *App) imagesExportError(w http.ResponseWriter, isHead bool, status int, code, message string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isHead {
		w.Header().Set("X-Export-Error", code)
		w.Header().Set("X-Export-Message", url.QueryEscape(message))
		w.WriteHeader(status)
		return
	}
	body, err := json.Marshal(map[string]any{
		"ok": false,
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
	if err != nil {
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (a *App) imagesExportLimited(w http.ResponseWriter, isHead bool, limit imagesExportLimit) {
	w.Header().Set("Retry-After", strconv.Itoa(limit.retryAfterSeconds))
	if limit.reason == "in_progress" {
		a.imagesExportError(w, isHead, http.StatusTooManyRequests, "EXPORT_IN_PROGRESS",
			imagesExportDescribeInProgress(limit.retryAfterSeconds))
		return
	}
	a.imagesExportError(w, isHead, http.StatusTooManyRequests, "EXPORT_COOLDOWN",
		imagesExportDescribeCooldown(limit.retryAfterSeconds, a.imagesExportCooldownHours()))
}

func imagesExportDescribeCooldown(retryAfterSeconds, cooldownHours int) string {
	remainingMinutes := max(1, int(math.Ceil(float64(retryAfterSeconds)/60)))
	hours := remainingMinutes / 60
	minutes := remainingMinutes % 60
	return fmt.Sprintf("导出过于频繁：每个账号每 %d 小时只能导出一次，请在 %d 小时 %d 分钟后重试。", cooldownHours, hours, minutes)
}

func imagesExportDescribeInProgress(retryAfterSeconds int) string {
	remainingMinutes := max(1, int(math.Ceil(float64(retryAfterSeconds)/60)))
	return fmt.Sprintf("该账号已有一个导出正在进行，请在约 %d 分钟后重试；若导出已中断，预约到期后会自动释放。", remainingMinutes)
}

func (a *App) imagesExportFail(w http.ResponseWriter, isHead bool, err error) {
	var requestErr *imagesError
	if errors.As(err, &requestErr) {
		a.imagesExportError(w, isHead, requestErr.status, requestErr.code, requestErr.message)
		return
	}
	log.Printf("images: export failed: %v", err)
	a.imagesExportError(w, isHead, http.StatusInternalServerError, "EXPORT_FAILED", "导出失败，请稍后重试")
}

// ---- 归档 ----

func imagesExportArchiveFileName(scope imagesExportScope, now time.Time) string {
	stamp := now.UTC().Format("20060102-150405")
	suffix := "all"
	if scope.mode == "ids" {
		suffix = "selected"
	}
	return fmt.Sprintf("picbox-images-%s-%s.zip", suffix, stamp)
}

func imagesBuildExportManifest(images []imagesExportImage, sizes map[string]int64, scope imagesExportScope, generatedAt time.Time) imagesExportManifest {
	entries := make([]imagesExportManifestEntry, 0, len(images))
	for _, image := range images {
		archivePath, contentType, err := imagesExportArchiveTarget(image)
		if err != nil {
			// 预检已经拒绝过未知格式；这里保底跳过而不是写入错误的 archivePath。
			continue
		}
		tags := image.Tags
		if tags == nil {
			tags = []string{}
		}
		entries = append(entries, imagesExportManifestEntry{
			ID:               image.ID,
			OriginalName:     image.OriginalName,
			Tags:             tags,
			CreatedAt:        image.CreatedAt,
			UpdatedAt:        image.UpdatedAt,
			IsVisible:        image.IsVisible,
			OriginalMimeType: image.OriginalMimeType,
			OriginalSize:     image.OriginalSize,
			ContentType:      contentType,
			Size:             sizes[image.ID],
			Width:            image.Width,
			Height:           image.Height,
			ArchivePath:      archivePath,
		})
	}
	total := int64(0)
	for _, entry := range entries {
		total += entry.Size
	}
	scopeName := "all"
	if scope.mode == "ids" {
		scopeName = "selected"
	}
	return imagesExportManifest{
		Version:     imagesExportVersion,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Scope:       scopeName,
		Count:       len(entries),
		TotalBytes:  total,
		Format:      "zip",
		Images:      entries,
	}
}

func imagesExportBuildReadme(manifest imagesExportManifest) string {
	scopeLabel := "所选图片 (selected)"
	if manifest.Scope == "all" {
		scopeLabel = "全部图片 (all)"
	}
	return strings.Join([]string{
		"PicBox 图片导出说明 / PicBox Image Export",
		"==========================================",
		"",
		"导出范围 / Scope: " + scopeLabel,
		"图片数量 / Images: " + strconv.Itoa(manifest.Count),
		"导出体积 / Exported bytes: " + strconv.FormatInt(manifest.TotalBytes, 10),
		"生成时间 / Generated at: " + manifest.GeneratedAt,
		"",
		"本压缩包内容 / Archive contents:",
		"- manifest.json：每张图片的元数据（id、文件名、标签、时间、可见性、原图 MIME 与大小、",
		"  归档内容的 MIME 与大小、宽高、归档路径）。",
		"- images/<id>.<ext>：PicBox 站内保存的「原尺寸」图片文件。",
		"- README.txt：本说明文件。",
		"",
		"重要说明 / Important:",
		"- 归档内文件严格等于站内保存的原图字节（不转码、不改写元信息），扩展名与 contentType",
		"  取自存储格式；manifest 中的 originalMimeType/originalSize 描述当前原图元数据。",
		"- 归档文件名使用图片 id，原始上传文件名仅记录在 manifest.json 中。",
		"- 隐藏（isVisible=false）图片也会由所属账号导出；其他账号的图片不会出现。",
		"",
	}, "\n")
}

// imagesExportContextReader 在每次读取前检查取消状态，让客户端断开能及时中断归档。
type imagesExportContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r imagesExportContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// imagesExportWriteArchive 写 README、manifest 与逐张原图。原图用 Store（图片本身已压缩）
// 并从磁盘流式复制，单张读完即关闭，因此内存占用与图片数量无关。
func (a *App) imagesExportWriteArchive(ctx context.Context, out io.Writer, images []imagesExportImage, sizes map[string]int64, scope imagesExportScope) error {
	generatedAt := time.Now().UTC()
	manifest := imagesBuildExportManifest(images, sizes, scope, generatedAt)
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode export manifest: %w", err)
	}

	archive := zip.NewWriter(out)
	if err := imagesExportAddBytes(archive, imagesExportReadmeName, []byte(imagesExportBuildReadme(manifest)), generatedAt); err != nil {
		return err
	}
	if err := imagesExportAddBytes(archive, imagesExportManifestName, manifestBody, generatedAt); err != nil {
		return err
	}
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.imagesExportAddFile(ctx, archive, image, sizes[image.ID]); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close export archive: %w", err)
	}
	return nil
}

func imagesExportAddBytes(archive *zip.Writer, name string, body []byte, modified time.Time) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified}
	header.SetMode(0o644)
	entry, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create archive entry %s: %w", name, err)
	}
	if _, err := entry.Write(body); err != nil {
		return fmt.Errorf("write archive entry %s: %w", name, err)
	}
	return nil
}

// imagesExportAddFile 复制单张原图。字节数必须与预检一致：文件在导出过程中被改动或
// 删除时宁可让本次导出失败并释放预约，也不产出与 manifest 不符的归档。
func (a *App) imagesExportAddFile(ctx context.Context, archive *zip.Writer, image imagesExportImage, size int64) error {
	archivePath, _, err := imagesExportArchiveTarget(image)
	if err != nil {
		return err
	}
	absolute, err := a.imagesSafePath(image.OriginalPath)
	if err != nil {
		return fmt.Errorf("resolve export path: %w", err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return fmt.Errorf("open export source %s: %w", image.ID, err)
	}
	defer file.Close()

	modified, ok := imagesParseTime(image.CreatedAt)
	if !ok {
		modified = time.Now().UTC()
	}
	header := &zip.FileHeader{Name: archivePath, Method: zip.Store, Modified: modified}
	header.SetMode(0o644)
	entry, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create archive entry %s: %w", archivePath, err)
	}
	written, err := io.Copy(entry, imagesExportContextReader{ctx: ctx, reader: io.LimitReader(file, size+1)})
	if err != nil {
		return fmt.Errorf("copy export source %s: %w", image.ID, err)
	}
	if written != size {
		return fmt.Errorf("图片字节在导出过程中发生变化: %s", image.ID)
	}
	return nil
}
