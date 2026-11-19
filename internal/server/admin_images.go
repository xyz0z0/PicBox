package server

// 管理端图片接口（UI：app/admin/AdminClient.tsx 的 AdminImagesTab）：
//
//	GET    /api/admin/images          游标分页 + 搜索（文件名/上传者）+ 可见性筛选
//	PATCH  /api/admin/images/{id}     operation=hide|show
//	DELETE /api/admin/images/{id}     管理员下架（deletedAt=now, deletedByAdmin=true）
//
// 共同边界（与原版 app/api/admin/images 一致）：
//   - 列表只含 deletedAt IS NULL 的图片，管理员已下架的不会再出现；
//   - PATCH/DELETE 只拒绝"已被管理员下架"的图片；上传者先放进自己回收站的图片仍可由
//     管理员隐藏/删除（原版同样允许）；
//   - 每次写入都在同一事务里写审计，审计失败整体回滚。

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

const (
	adminOperationHide = "hide"
	adminOperationShow = "show"
)

type adminImageUploader struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type adminImageItem struct {
	ID           string             `json:"id"`
	OriginalName string             `json:"originalName"`
	CreatedAt    string             `json:"createdAt"`
	IsVisible    bool               `json:"isVisible"`
	Uploader     adminImageUploader `json:"uploader"`
	URL          string             `json:"url"`
	PageURL      string             `json:"pageUrl"`
	ThumbURL     string             `json:"thumbUrl"`
}

type adminImagesListResponse struct {
	OK         bool             `json:"ok"`
	Items      []adminImageItem `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

func (a *App) adminHandleImagesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r); !ok {
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	paging := adminParsePaging(query, adminImagesDefaultLimit, adminMaxSearchLength)

	visibility := strings.TrimSpace(query.Get("visibility"))
	if visibility == "" {
		visibility = "all"
	}
	conditions := []string{`i.deletedAt IS NULL`}
	args := make([]any, 0, 4)
	switch visibility {
	case "all":
	case "visible":
		conditions = append(conditions, `i.isVisible = 1`)
	case "hidden":
		conditions = append(conditions, `i.isVisible = 0`)
	default:
		writeError(w, http.StatusBadRequest, "INVALID_VISIBILITY", "visibility must be all, visible, or hidden")
		return
	}

	if paging.Query != "" {
		pattern := "%" + imagesLikeEscape(paging.Query) + "%"
		conditions = append(conditions,
			`(i.originalName LIKE ? ESCAPE '\' OR uploader.email LIKE ? ESCAPE '\' OR uploader.username LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern)
	}

	// 复合游标 (createdAt, id)：同一秒上传的多张图片在页边界不会重复或丢失。
	// 旧的纯时间戳游标也继续接受（只按 createdAt 比较），与图片列表一致。
	if createdAt, id, ok := adminParseCursor(query.Get("cursor")); ok {
		if id != "" {
			conditions = append(conditions, `(i.createdAt < ? OR (i.createdAt = ? AND i.id < ?))`)
			args = append(args, createdAt, createdAt, id)
		} else {
			conditions = append(conditions, `i.createdAt < ?`)
			args = append(args, createdAt)
		}
	}

	statement := `SELECT i.id, i.originalName, i.createdAt, i.isVisible,
		uploader.id, uploader.email, uploader.username
		FROM "Image" i JOIN "User" uploader ON uploader.id = i.uploaderId
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY i.createdAt DESC, i.id DESC LIMIT ?`
	queryArgs := append(append([]any{}, args...), paging.Limit)

	rows, err := a.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGES_FAILED", "读取图片列表失败")
		return
	}
	items := make([]adminImageItem, 0, paging.Limit)
	for rows.Next() {
		var (
			item      adminImageItem
			isVisible int
		)
		if err := rows.Scan(&item.ID, &item.OriginalName, &item.CreatedAt, &isVisible,
			&item.Uploader.ID, &item.Uploader.Email, &item.Uploader.Username); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "IMAGES_FAILED", "读取图片列表失败")
			return
		}
		item.IsVisible = isVisible != 0
		item.URL, item.PageURL, item.ThumbURL = a.imagesURLs(item.ID)
		items = append(items, item)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		writeError(w, http.StatusInternalServerError, "IMAGES_FAILED", "读取图片列表失败")
		return
	}

	var nextCursor *string
	if len(items) == paging.Limit && len(items) > 0 {
		last := items[len(items)-1]
		cursor := adminEncodeCursor(last.CreatedAt, last.ID)
		nextCursor = &cursor
	}

	writeJSON(w, http.StatusOK, adminImagesListResponse{OK: true, Items: items, NextCursor: nextCursor})
}

func (a *App) adminHandleImageUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	body := adminLooseJSONObject(r)
	operation, _ := adminStringField(body, "operation")
	operation = strings.TrimSpace(operation)
	if operation != adminOperationHide && operation != adminOperationShow {
		writeError(w, http.StatusBadRequest, "INVALID_OPERATION", "operation must be hide or show")
		return
	}

	imageID := strings.TrimSpace(r.PathValue("id"))
	if imageID == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Image not found")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_UPDATE_FAILED", "更新图片状态失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var (
		isVisible      int
		deletedByAdmin int
	)
	err = tx.QueryRowContext(ctx,
		`SELECT isVisible, deletedByAdmin FROM "Image" WHERE id = ?`, imageID).
		Scan(&isVisible, &deletedByAdmin)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedByAdmin != 0) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Image not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_UPDATE_FAILED", "更新图片状态失败，请稍后重试")
		return
	}

	nextVisible := operation == adminOperationShow
	if _, err := tx.ExecContext(ctx,
		`UPDATE "Image" SET isVisible = ?, updatedAt = ? WHERE id = ?`,
		imagesBoolInt(nextVisible), imagesNow(), imageID); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_UPDATE_FAILED", "更新图片状态失败，请稍后重试")
		return
	}

	action := auditActionAdminImageHide
	if nextVisible {
		action = auditActionAdminImageShow
	}
	if err := auditWrite(ctx, tx, actor.ID, action, nil, "imageId="+imageID); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_UPDATE_FAILED", "更新图片状态失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_UPDATE_FAILED", "更新图片状态失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"image": map[string]any{
			"id":        imageID,
			"isVisible": nextVisible,
		},
	})
}

func (a *App) adminHandleImageDelete(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	imageID := strings.TrimSpace(r.PathValue("id"))
	if imageID == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Image not found")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_DELETE_FAILED", "删除图片失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var deletedByAdmin int
	err = tx.QueryRowContext(ctx,
		`SELECT deletedByAdmin FROM "Image" WHERE id = ?`, imageID).Scan(&deletedByAdmin)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedByAdmin != 0) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Image not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_DELETE_FAILED", "删除图片失败，请稍后重试")
		return
	}

	now := imagesNow()
	// 管理员下架：deletedByAdmin=true 保证不会出现在上传者的回收站里。
	if _, err := tx.ExecContext(ctx,
		`UPDATE "Image" SET deletedAt = ?, deletedByAdmin = 1, updatedAt = ? WHERE id = ?`,
		now, now, imageID); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_DELETE_FAILED", "删除图片失败，请稍后重试")
		return
	}
	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminImageDelete, nil, "imageId="+imageID); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_DELETE_FAILED", "删除图片失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "IMAGE_DELETE_FAILED", "删除图片失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
