package server

// 本文件补齐原版工作台剩余接口中的四组：
//
//	GET   /api/images/tags      标签候选（本人未删除图片的标签并集）
//	GET   /api/images/usage     个人配额用量
//	PATCH /api/images/batch     批量可见性 / 软删除 / 恢复 / 彻底删除 / 改标签
//	POST  /api/images/from-url  URL 导入（复用真实上传校验与配额）
//
// 导出在 images_export.go，单图元数据在 images_meta.go，回收站保留期清理在
// images_retention.go。四个文件共用 core 提供的 Config 字段与配额 helper。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	imagesBatchMaxIDs    = 500
	imagesBatchActionTag = "tag"

	imagesRemoteURLMaxLength  = 8192
	imagesRemoteMaxRedirects  = 3
	imagesRemoteDialTimeout   = 10 * time.Second
	imagesRemoteHeaderTimeout = 15 * time.Second
	imagesRemoteOverall       = 30 * time.Second
)

// registerImagesExtraRoutes 由 core 在 New 中调用，注册本任务的全部补全接口。
//
// 全部使用标准 mux.HandleFunc 字面量/通配路径：
//   - 字面量路径（tags/usage/export/batch/from-url）比 images.go 的 /api/images/{id}
//     更具体，因此不会被当成 id 处理；
//   - "GET ..." 模式同时匹配 HEAD（ServeMux 语义），导出 handler 自己按 r.Method 区分；
//   - images.go 不再重复注册 "HEAD /api/images/{id}"，否则 GET 模式隐含的 HEAD 会与这些
//     字面量 GET 路径在方法集上冲突，注册阶段直接 panic。
func (a *App) registerImagesExtraRoutes() {
	a.mux.HandleFunc("GET /api/images/tags", a.imagesHandleTags)
	a.mux.HandleFunc("GET /api/images/usage", a.imagesHandleUsage)
	a.mux.HandleFunc("GET /api/images/export", a.imagesHandleExport)
	a.mux.HandleFunc("PATCH /api/images/batch", a.imagesHandleBatch)
	a.mux.HandleFunc("POST /api/images/from-url", a.imagesHandleFromURL)
	a.mux.HandleFunc("GET /api/images/{id}/meta", a.imagesHandleMeta)
}

// ---- 公共 JSON 读取 ----

// imagesReadJSONObject 读取有界 JSON 对象。body 不是对象（数组、标量、null）时按
// 原版接口统一报 INVALID_REQUEST，而不是把零值当成请求。
func imagesReadJSONObject(r *http.Request) (map[string]json.RawMessage, error) {
	invalid := &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "Expected a JSON object"}
	body, err := io.ReadAll(io.LimitReader(r.Body, imagesMaxRequestJSON+1))
	if err != nil {
		return nil, invalid
	}
	if int64(len(body)) > imagesMaxRequestJSON {
		return nil, invalid
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, invalid
	}
	return raw, nil
}

// imagesRawBool 只接受 JSON 的 true/false 字面量。null、数字、字符串都拒绝：
// json.Unmarshal(null, &bool) 会成功并留下 false，把隐藏图片当成显式请求。
func imagesRawBool(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// imagesRawStringArray 只接受字符串数组；null 或混入非字符串一律拒绝。
func imagesRawStringArray(raw json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var values []string
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, false
	}
	return values, true
}

// ---- 标签候选 ----

type imagesTagsResponse struct {
	OK   bool     `json:"ok"`
	Tags []string `json:"tags"`
}

// imagesHandleTags 返回本人未删除图片的标签并集。原版 UI 用它填充上传建议与筛选器。
func (a *App) imagesHandleTags(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT DISTINCT t.name FROM "ImageTag" t JOIN "Image" i ON i.id = t.imageId
		WHERE i.uploaderId = ? AND i.deletedAt IS NULL
		ORDER BY t.name`, user.ID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	tags := make([]string, 0, 16)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			imagesFail(w, err)
			return
		}
		tags = append(tags, name)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		imagesFail(w, scanErr)
		return
	}
	writeJSON(w, http.StatusOK, imagesTagsResponse{OK: true, Tags: tags})
}

// ---- 配额用量 ----

type imagesUsageResponse struct {
	OK         bool  `json:"ok"`
	UsedBytes  int64 `json:"usedBytes"`
	QuotaBytes int64 `json:"quotaBytes"`
}

// imagesHandleUsage 返回与上传校验完全一致的用量语义：回收站与管理员下架但尚未
// 物理删除的图片都计入 usedBytes，0 表示不限额。
func (a *App) imagesHandleUsage(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	used, err := a.quotaUsedBytes(r.Context(), user.ID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	quota, err := a.quotaEffectiveBytes(r.Context(), user.ID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imagesUsageResponse{OK: true, UsedBytes: used, QuotaBytes: quota})
}

// ---- 批量操作 ----

type imagesBatchRequest struct {
	ids     []string
	action  string
	visible bool
	tags    []string
	mode    string
}

type imagesBatchResponse struct {
	OK             bool  `json:"ok"`
	UpdatedCount   int64 `json:"updatedCount"`
	CleanupPending int64 `json:"cleanupPending,omitempty"`
	FailedCount    int64 `json:"failedCount,omitempty"`
	// UpdatedIDs is filled only for purge: the ids whose database rows were
	// really removed, including entries whose disk cleanup is still pending.
	// The UI must remove exactly these ids and keep failed ones selected, so a
	// partial failure cannot make a still-existing image disappear. Other
	// actions (and the original Next backend) do not provide the field; the
	// client then keeps its legacy "all requested ids" behavior.
	UpdatedIDs []string `json:"updatedIds"`
}

func imagesBatchActionSupportsTagMode(mode string) bool {
	switch mode {
	case "add", "remove", "replace":
		return true
	}
	return false
}

// imagesParseBatchRequest 全量校验请求体，任何一项非法都在写入之前失败。
func imagesParseBatchRequest(raw map[string]json.RawMessage) (imagesBatchRequest, error) {
	request := imagesBatchRequest{mode: "replace"}

	rawIDs, present := raw["ids"]
	if !present {
		return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_IDS", message: "ids must be a non-empty array of strings"}
	}
	ids, ok := imagesRawStringArray(rawIDs)
	if !ok || len(ids) == 0 {
		return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_IDS", message: "ids must be a non-empty array of strings"}
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_IDS", message: "ids must be a non-empty array of strings"}
		}
	}
	if len(ids) > imagesBatchMaxIDs {
		return request, &imagesError{
			status:  http.StatusBadRequest,
			code:    "BATCH_TOO_LARGE",
			message: fmt.Sprintf("A batch can contain at most %d images", imagesBatchMaxIDs),
		}
	}

	actionRaw, present := raw["action"]
	if !present || json.Unmarshal(actionRaw, &request.action) != nil {
		return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_ACTION", message: "action must be visibility, delete, restore, purge, or tag"}
	}
	switch request.action {
	case "visibility", "delete", "restore", "purge", imagesBatchActionTag:
	default:
		return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_ACTION", message: "action must be visibility, delete, restore, purge, or tag"}
	}

	if request.action == "visibility" {
		visibleRaw, present := raw["visible"]
		value, ok := imagesRawBool(visibleRaw)
		if !present || !ok {
			return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_VISIBILITY", message: "visible must be boolean for visibility actions"}
		}
		request.visible = value
	}

	if request.action == imagesBatchActionTag {
		if _, hasTags := raw["tags"]; hasTags {
			if _, hasTag := raw["tag"]; hasTag {
				// 新旧字段永不隐式合并：同时出现时无法判断调用者意图。
				return request, imagesTagError()
			}
			values, ok := imagesRawStringArray(raw["tags"])
			if !ok {
				return request, imagesTagError()
			}
			normalized, err := imagesNormalizeTags(values)
			if err != nil {
				return request, err
			}
			request.tags = normalized
		} else if tagRaw, hasTag := raw["tag"]; hasTag {
			var value string
			if len(bytes.TrimSpace(tagRaw)) == 0 || bytes.Equal(bytes.TrimSpace(tagRaw), []byte("null")) || json.Unmarshal(tagRaw, &value) != nil {
				return request, imagesTagError()
			}
			normalized, err := imagesNormalizeTags([]string{value})
			if err != nil {
				return request, err
			}
			request.tags = normalized
		} else {
			return request, imagesTagError()
		}

		if modeRaw, present := raw["mode"]; present {
			// 显式 null 不能被当成"未提供"而静默回落到 replace。
			if bytes.Equal(bytes.TrimSpace(modeRaw), []byte("null")) || json.Unmarshal(modeRaw, &request.mode) != nil {
				return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG_MODE", message: "mode must be add, remove, or replace; legacy tag only supports replace"}
			}
		}
		if !imagesBatchActionSupportsTagMode(request.mode) {
			return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG_MODE", message: "mode must be add, remove, or replace; legacy tag only supports replace"}
		}
		if _, hasTag := raw["tag"]; hasTag && request.mode != "replace" {
			return request, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG_MODE", message: "mode must be add, remove, or replace; legacy tag only supports replace"}
		}
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	request.ids = unique
	return request, nil
}

// imagesBatchScope 构造批量作用域：回收站动作只匹配本人回收站记录，其余只匹配
// 本人未删除记录。任何 id 不在作用域内都整体拒绝，不做部分成功。
func imagesBatchScope(trash bool, userID int64, ids []string) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	clause := "uploaderId = ? AND id IN (" + placeholders + ")"
	if trash {
		return clause + " AND deletedAt IS NOT NULL AND deletedByAdmin = 0", args
	}
	return clause + " AND deletedAt IS NULL", args
}

func imagesBatchIsTrashAction(action string) bool {
	return action == "restore" || action == "purge"
}

func (a *App) imagesHandleBatch(w http.ResponseWriter, r *http.Request) {
	// 先解析出 action 再鉴权：restore/purge 与单张 /restore、?permanent=true 一样只接受
	// 登录 Cookie，API Key 不能把记录放回图库，更不能物理删除字节。其它 action
	// （visibility/delete/tag）保持原契约，允许 Bearer。请求体是受限的 64KB JSON 解析，
	// 不触碰数据库或文件，因此在鉴权前读取没有副作用。
	raw, err := imagesReadJSONObject(r)
	if err != nil {
		imagesFail(w, err)
		return
	}
	request, err := imagesParseBatchRequest(raw)
	if err != nil {
		imagesFail(w, err)
		return
	}
	user, ok := a.authenticate(w, r, !imagesBatchIsTrashAction(request.action))
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if request.action == "purge" {
		a.imagesHandleBatchPurge(w, r, user, request)
		return
	}
	if request.action == imagesBatchActionTag {
		updated, err := a.imagesBatchUpdateTags(r.Context(), user.ID, request)
		if err != nil {
			imagesFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, imagesBatchResponse{OK: true, UpdatedCount: updated})
		return
	}
	updated, err := a.imagesBatchUpdateState(r.Context(), user.ID, request)
	if err != nil {
		imagesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imagesBatchResponse{OK: true, UpdatedCount: updated})
}

// imagesBatchUpdateState 处理 visibility / delete / restore：同一事务内先统计
// 作用域内的行数再更新，因此不存在"部分越权却成功"的窗口。
func (a *App) imagesBatchUpdateState(ctx context.Context, userID int64, request imagesBatchRequest) (int64, error) {
	clause, args := imagesBatchScope(imagesBatchIsTrashAction(request.action), userID, request.ids)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin batch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var owned int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM \"Image\" WHERE "+clause, args...).Scan(&owned); err != nil {
		return 0, fmt.Errorf("count batch images: %w", err)
	}
	if owned != int64(len(request.ids)) {
		return 0, &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "One or more images cannot be modified"}
	}

	if request.action == "restore" {
		// 与单张恢复一致：字节缺失时整体 409，绝不把"永远取不到字节"的记录放回图库。
		if err := a.imagesBatchVerifyRestoreFiles(ctx, tx, clause, args, len(request.ids)); err != nil {
			return 0, err
		}
	}

	now := imagesNow()
	setSQL := "updatedAt = ?"
	setArgs := []any{now}
	switch request.action {
	case "visibility":
		setSQL = "isVisible = ?, updatedAt = ?"
		setArgs = []any{imagesBoolInt(request.visible), now}
	case "delete":
		setSQL = "deletedAt = ?, updatedAt = ?"
		setArgs = []any{now, now}
	case "restore":
		setSQL = "deletedAt = NULL, updatedAt = ?"
	}
	updateArgs := append(append([]any{}, setArgs...), args...)
	result, err := tx.ExecContext(ctx, "UPDATE \"Image\" SET "+setSQL+" WHERE "+clause, updateArgs...)
	if err != nil {
		return 0, fmt.Errorf("update batch images: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count updated batch images: %w", err)
	}
	if affected != int64(len(request.ids)) {
		// 事务内已经核对过行数；走到这里说明存储层行为异常，回滚而不是报告部分成功。
		return 0, &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "One or more images cannot be modified"}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit batch update: %w", err)
	}
	return affected, nil
}

// imagesBatchVerifyRestoreFiles 在恢复事务内确认每个目标的原图仍然存在且是常规文件。
// 与 imagesRestoreTrashed 使用同一个 helper，因此单张与批量的错误码/语义一致
// （缺失 → 409 IMAGE_BYTES_MISSING）。任何一个缺失都让整批回滚，其余记录仍在回收站。
func (a *App) imagesBatchVerifyRestoreFiles(ctx context.Context, tx *sql.Tx, clause string, args []any, expected int) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, originalPath FROM "Image" WHERE `+clause, args...)
	if err != nil {
		return fmt.Errorf("read restore images: %w", err)
	}
	type restoreTarget struct {
		id           string
		originalPath string
	}
	targets := make([]restoreTarget, 0, expected)
	for rows.Next() {
		var target restoreTarget
		if err := rows.Scan(&target.id, &target.originalPath); err != nil {
			rows.Close()
			return fmt.Errorf("scan restore image: %w", err)
		}
		targets = append(targets, target)
	}
	scanErr := rows.Err()
	// 单连接驱动：stat 之前必须关闭结果集。
	rows.Close()
	if scanErr != nil {
		return fmt.Errorf("read restore images: %w", scanErr)
	}
	if len(targets) != expected {
		return &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "One or more images cannot be modified"}
	}
	for _, target := range targets {
		absolute, err := a.imagesSafePath(target.originalPath)
		if err != nil {
			return fmt.Errorf("resolve original path for restore: %w", err)
		}
		if err := imagesRequireRegularFile(absolute); err != nil {
			return err
		}
	}
	return nil
}

// imagesBatchUpdateTags 在一个事务里读取每张图片现有标签、按 add/remove/replace
// 合并并整体替换。合并结果统一走标签校验（去重、排序、≤20×64、控制字符拒绝）。
func (a *App) imagesBatchUpdateTags(ctx context.Context, userID int64, request imagesBatchRequest) (int64, error) {
	clause, args := imagesBatchScope(false, userID, request.ids)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin batch tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, "SELECT id FROM \"Image\" WHERE "+clause, args...)
	if err != nil {
		return 0, fmt.Errorf("read batch tag images: %w", err)
	}
	matched := make([]string, 0, len(request.ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan batch tag image: %w", err)
		}
		matched = append(matched, id)
	}
	scanErr := rows.Err()
	// 单连接驱动：读取下一组查询前必须关闭结果集。
	rows.Close()
	if scanErr != nil {
		return 0, fmt.Errorf("read batch tag images: %w", scanErr)
	}
	if len(matched) != len(request.ids) {
		return 0, &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "One or more images cannot be modified"}
	}

	current, err := imagesLoadTagsFor(ctx, tx, matched)
	if err != nil {
		return 0, err
	}

	updates := make(map[string][]string, len(matched))
	for _, id := range matched {
		merged, err := imagesMergeTags(current[id], request.tags, request.mode)
		if err != nil {
			return 0, err
		}
		updates[id] = merged
	}

	now := imagesNow()
	for _, id := range matched {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "ImageTag" WHERE imageId = ?`, id); err != nil {
			return 0, fmt.Errorf("delete image tags: %w", err)
		}
		for _, name := range updates[id] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO "ImageTag" (imageId, name) VALUES (?, ?)`, id, name); err != nil {
				return 0, fmt.Errorf("insert image tag: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "Image" SET updatedAt = ? WHERE id = ? AND uploaderId = ? AND deletedAt IS NULL`, now, id, userID); err != nil {
			return 0, fmt.Errorf("touch image: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit batch tag update: %w", err)
	}
	return int64(len(matched)), nil
}

// imagesMergeTags 实现原版 lib/imageTag.ts 的 mergeImageTags 语义，并对合并结果
// 重新做完整校验：add 可能突破 20 个标签上限，必须显式报错而不是截断。
func imagesMergeTags(current, incoming []string, mode string) ([]string, error) {
	switch mode {
	case "add":
		combined := make([]string, 0, len(current)+len(incoming))
		combined = append(combined, current...)
		combined = append(combined, incoming...)
		return imagesNormalizeTags(combined)
	case "remove":
		drop := make(map[string]struct{}, len(incoming))
		for _, tag := range incoming {
			drop[tag] = struct{}{}
		}
		kept := make([]string, 0, len(current))
		for _, tag := range current {
			if _, dropped := drop[tag]; dropped {
				continue
			}
			kept = append(kept, tag)
		}
		return imagesNormalizeTags(kept)
	default:
		return imagesNormalizeTags(incoming)
	}
}

type imagesQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// imagesLoadTagsFor 一次读出一批图片的标签。调用方必须先关闭其它结果集。
func imagesLoadTagsFor(ctx context.Context, querier imagesQuerier, ids []string) (map[string][]string, error) {
	tags := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return tags, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := querier.QueryContext(ctx,
		`SELECT imageId, name FROM "ImageTag" WHERE imageId IN (`+placeholders+`) ORDER BY imageId, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("read image tags: %w", err)
	}
	for rows.Next() {
		var imageID, name string
		if err := rows.Scan(&imageID, &name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan image tag: %w", err)
		}
		tags[imageID] = append(tags[imageID], name)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("read image tags: %w", scanErr)
	}
	return tags, nil
}

// imagesHandleBatchPurge 先整体核对作用域，再逐张走持久化清单的彻底删除流程。
// updatedCount 只统计真正删掉的行；物理清理未完成的条目单独计数，绝不假称已从磁盘移除。
func (a *App) imagesHandleBatchPurge(w http.ResponseWriter, r *http.Request, user *User, request imagesBatchRequest) {
	ctx := r.Context()
	clause, args := imagesBatchScope(true, user.ID, request.ids)
	var owned int64
	if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM \"Image\" WHERE "+clause, args...).Scan(&owned); err != nil {
		imagesFail(w, fmt.Errorf("count batch purge images: %w", err))
		return
	}
	if owned != int64(len(request.ids)) {
		imagesFail(w, &imagesError{status: http.StatusForbidden, code: "FORBIDDEN", message: "One or more images cannot be modified"})
		return
	}

	response := imagesBatchPurgeResults(request.ids, func(id string) (bool, error) {
		return a.imagesPurgeTrashed(ctx, user.ID, id)
	})
	writeJSON(w, http.StatusOK, response)
}

// imagesBatchPurgeResults aggregates per-image purge outcomes. Every id whose
// row was deleted is reported in UpdatedIDs, including cleanupPending ones; a
// per-image failure only bumps FailedCount and is never claimed as removed.
func imagesBatchPurgeResults(ids []string, purge func(string) (bool, error)) imagesBatchResponse {
	response := imagesBatchResponse{OK: true, UpdatedIDs: make([]string, 0, len(ids))}
	for _, id := range ids {
		cleanupPending, err := purge(id)
		if err != nil {
			// 单张失败不影响其余图片：记录真实失败数量，不把它算进 updatedCount。
			logImagesBatchPurgeFailure(id, err)
			response.FailedCount++
			continue
		}
		response.UpdatedCount++
		response.UpdatedIDs = append(response.UpdatedIDs, id)
		if cleanupPending {
			response.CleanupPending++
		}
	}
	return response
}

func logImagesBatchPurgeFailure(imageID string, err error) {
	log.Printf("images: batch purge failed for %s: %v", imageID, err)
}

// ---- URL 导入 ----

// imagesHandleFromURL 复用真实上传路径：单处理槽、流式限量临时文件、格式嗅探、
// 解码/缩略图、事务内配额复核与落盘。这里只负责"把远端字节安全地取到临时文件"。
func (a *App) imagesHandleFromURL(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	// 与 multipart 上传共用同一个限流（core 提供，按可信 clientIP 100 次/小时）。
	// 放在读取请求体、下载与单处理槽之前：超限请求不消耗出网流量，也不占用上传槽。
	if !a.limitImageUpload(w, r) {
		return
	}
	raw, err := imagesReadJSONObject(r)
	if err != nil {
		imagesFail(w, err)
		return
	}

	var rawURL string
	urlRaw, present := raw["url"]
	if !present || bytes.Equal(bytes.TrimSpace(urlRaw), []byte("null")) || json.Unmarshal(urlRaw, &rawURL) != nil {
		imagesFail(w, imagesInvalidURLError())
		return
	}
	if _, err := imagesParseRemoteURL(rawURL); err != nil {
		imagesFail(w, err)
		return
	}

	tags := []string{}
	if tagsRaw, hasTags := raw["tags"]; hasTags {
		values, ok := imagesRawStringArray(tagsRaw)
		if !ok {
			imagesFail(w, imagesTagError())
			return
		}
		normalized, err := imagesNormalizeTags(values)
		if err != nil {
			imagesFail(w, err)
			return
		}
		tags = normalized
	}

	// 配额已满时不要先去下载远端文件；剩余空间不足由 imagesStoreUpload 精确校验。
	quota, err := a.quotaEffectiveBytes(r.Context(), user.ID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	if quota > 0 {
		used, err := a.quotaUsedBytes(r.Context(), user.ID)
		if err != nil {
			imagesFail(w, err)
			return
		}
		if used >= quota {
			imagesFail(w, imagesQuotaError(used, quota))
			return
		}
	}

	release, ok := a.imagesTryLockUpload()
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "UPLOAD_BUSY", "已有图片正在处理，请稍后重试")
		return
	}
	defer release()

	upload, err := a.imagesDownloadRemote(r.Context(), rawURL, a.imagesMaxUploadBytes())
	if err != nil {
		imagesFail(w, err)
		return
	}
	defer upload.cleanup()
	upload.tags = tags

	response, err := a.imagesStoreUpload(r.Context(), user.ID, upload)
	if err != nil {
		imagesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func imagesInvalidURLError() *imagesError {
	return &imagesError{status: http.StatusBadRequest, code: "INVALID_URL", message: "请输入有效的 HTTP/HTTPS 图片直链"}
}

// imagesParseRemoteURL 只接受不含认证信息的 http/https 直链，长度与原版一致。
func imagesParseRemoteURL(value string) (*url.URL, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(value) > imagesRemoteURLMaxLength {
		return nil, imagesInvalidURLError()
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, imagesInvalidURLError()
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_URL", message: "仅支持不含认证信息的 HTTP/HTTPS 图片直链"}
	}
	if parsed.User != nil || parsed.Host == "" {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_URL", message: "仅支持不含认证信息的 HTTP/HTTPS 图片直链"}
	}
	if parsed.Fragment != "" {
		parsed.Fragment = ""
		parsed.RawFragment = ""
	}
	return parsed, nil
}

var (
	errImagesRemoteUnsafeAddress   = errors.New("图片链接必须指向公网地址")
	errImagesRemoteRedirectTooMany = errors.New("图片链接重定向无效或超过 3 次")
)

// imagesRemoteBlockedPrefixes 覆盖原版 lib/remoteImage.ts 的 BlockList，并补上云元数据
// 与保留网段。IsGlobalUnicast 之外仍然逐条比对，因为私网地址也是 global unicast。
var imagesRemoteBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// imagesRemoteAddressAllowed 判定一个已解析的地址能否作为远端图片源。
func imagesRemoteAddressAllowed(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		// 覆盖环回、链路本地、组播、未指定地址。
		return false
	}
	for _, prefix := range imagesRemoteBlockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// imagesRemoteResolve 解析主机名。IP 字面量直接返回，避免无谓的 DNS 查询。
func imagesRemoteResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{address}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("域名没有解析结果: %s", host)
	}
	return addresses, nil
}

var imagesRemoteDialer = &net.Dialer{Timeout: imagesRemoteDialTimeout, KeepAlive: -1}

// imagesRemoteDialContext 在拨号前解析并校验每一个地址，然后直接对校验过的 IP 建立
// 连接。这样"解析校验"和"实际连接"用的是同一份结果，DNS 重绑定无法绕过校验。
func imagesRemoteDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("远端地址非法: %w", err)
	}
	addresses, err := imagesRemoteResolve(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, resolved := range addresses {
		if !imagesRemoteAddressAllowed(resolved) {
			return nil, fmt.Errorf("%w: %s", errImagesRemoteUnsafeAddress, resolved.String())
		}
	}
	return imagesRemoteDialer.DialContext(ctx, "tcp", net.JoinHostPort(addresses[0].Unmap().String(), port))
}

// imagesRemoteClientFactory 是本包唯一构造外发客户端的位置。测试把它替换成指向
// httptest 环回服务器的客户端；生产路径本身（含 SSRF 拨号校验）由 imagesRemoteNewClient
// 提供，测试会直接断言它的拨号器拒绝环回/私网/元数据地址。
var imagesRemoteClientFactory = imagesRemoteNewClient

// imagesRemoteNewClient 构造一个不跟随任意重定向、不做内容压缩、带总时限的客户端。
func imagesRemoteNewClient() *http.Client {
	transport := &http.Transport{
		// 不走环境代理：代理会让拨号校验失效。
		Proxy:                  nil,
		DialContext:            imagesRemoteDialContext,
		DisableCompression:     true,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSHandshakeTimeout:    imagesRemoteDialTimeout,
		ResponseHeaderTimeout:  imagesRemoteHeaderTimeout,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   imagesRemoteOverall,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= imagesRemoteMaxRedirects {
				return errImagesRemoteRedirectTooMany
			}
			// 每一跳都必须重新通过 scheme / 认证信息校验；IP 校验在拨号时逐跳执行。
			if _, err := imagesParseRemoteURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}

// imagesDownloadRemote 把远端字节流式写入 data/tmp 的临时文件，超过上限立即失败。
// 全程不缓存整张图片；失败与取消都由调用方的 defer cleanup() 删除临时文件。
func (a *App) imagesDownloadRemote(ctx context.Context, rawURL string, maxBytes int64) (*imagesUpload, error) {
	parsed, err := imagesParseRemoteURL(rawURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return nil, &imagesError{status: http.StatusInternalServerError, code: "IMAGE_REQUEST_FAILED", message: "服务数据目录未配置"}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, imagesInvalidURLError()
	}
	request.Header.Set("Accept", "image/*")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "PicBox/1 (+image-import)")

	client := imagesRemoteClientFactory()
	response, err := client.Do(request)
	if err != nil {
		return nil, imagesRemoteDownloadError(ctx, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, &imagesError{
			status:  http.StatusBadRequest,
			code:    "DOWNLOAD_FAILED",
			message: fmt.Sprintf("图片下载失败（HTTP %d）", response.StatusCode),
		}
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_ENCODING", message: "图片服务器返回了不支持的压缩传输格式"}
	}
	if response.ContentLength > maxBytes {
		return nil, imagesRemoteTooLargeError()
	}

	tempDir := filepath.Join(a.cfg.DataDir, imagesTempDirName)
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		return nil, fmt.Errorf("create remote import temp dir: %w", err)
	}
	tempFile, err := os.CreateTemp(tempDir, "remote-*.part")
	if err != nil {
		return nil, fmt.Errorf("create remote import temp file: %w", err)
	}
	tempPath := tempFile.Name()
	discard := func() {
		_ = tempFile.Close()
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			logImagesCleanup("remote import", tempPath, removeErr)
		}
	}

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tempFile, hasher), io.LimitReader(response.Body, maxBytes+1))
	if copyErr != nil {
		discard()
		return nil, imagesRemoteDownloadError(ctx, copyErr)
	}
	if written > maxBytes {
		discard()
		return nil, imagesRemoteTooLargeError()
	}
	if written == 0 {
		discard()
		return nil, &imagesError{status: http.StatusBadRequest, code: "DOWNLOAD_FAILED", message: "无法下载图片，请确认链接可直接访问"}
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("close remote import: %w", err)
	}

	name := imagesRemoteOriginalName(parsed)
	if response.Request != nil && response.Request.URL != nil {
		name = imagesRemoteOriginalName(response.Request.URL)
	}
	return &imagesUpload{
		tempPath: tempPath,
		name:     name,
		size:     written,
		sha256:   hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func imagesRemoteTooLargeError() *imagesError {
	return &imagesError{status: http.StatusRequestEntityTooLarge, code: "FILE_TOO_LARGE", message: "远程图片超过上传大小限制"}
}

// imagesRemoteDownloadError 区分取消/超时与普通网络失败，避免把 30 秒超时报成"链接无效"。
func imagesRemoteDownloadError(ctx context.Context, err error) error {
	if errors.Is(err, errImagesRemoteUnsafeAddress) {
		return &imagesError{status: http.StatusBadRequest, code: "UNSAFE_URL", message: errImagesRemoteUnsafeAddress.Error()}
	}
	if errors.Is(err, errImagesRemoteRedirectTooMany) {
		return &imagesError{status: http.StatusBadRequest, code: "INVALID_REDIRECT", message: errImagesRemoteRedirectTooMany.Error()}
	}
	var requestErr *imagesError
	if errors.As(err, &requestErr) {
		return requestErr
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &imagesError{status: http.StatusRequestTimeout, code: "DOWNLOAD_TIMEOUT", message: "图片下载超时或已取消，请重试"}
	}
	return &imagesError{status: http.StatusBadRequest, code: "DOWNLOAD_FAILED", message: "无法下载图片，请确认链接可直接访问"}
}

// imagesRemoteOriginalName 从最终 URL 路径取展示用文件名，并复用上传路径的清洗规则。
func imagesRemoteOriginalName(finalURL *url.URL) string {
	if finalURL == nil {
		return "remote-image"
	}
	base := path.Base(finalURL.Path)
	if base == "" || base == "." || base == "/" {
		return "remote-image"
	}
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	name := imagesCleanFilename(base)
	if name == "upload" {
		// imagesCleanFilename 的兜底值面向本地上传；远端导入用自己的兜底名。
		return "remote-image"
	}
	return name
}
