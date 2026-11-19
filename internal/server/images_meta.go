package server

// GET /api/images/{id}/meta 是原版服务端渲染详情页（/images/{id}）的新版内部契约：
// Go 单二进制下页面是静态壳，页面需要的元数据由这个接口提供。
//
// 可见性规则与原版详情页一致：
//   - 记录不存在、已被软删除或被管理员下架 → 404（不泄露存在性）；
//   - 隐藏图片只有上传者本人的 Cookie 会话可以读到元数据，其他调用者一律 404。

import (
	"database/sql"
	"errors"
	"net/http"
)

type imagesMetaImage struct {
	ID               string   `json:"id"`
	OriginalName     string   `json:"originalName"`
	OriginalMimeType string   `json:"originalMimeType"`
	Size             int64    `json:"size"`
	Width            int64    `json:"width"`
	Height           int64    `json:"height"`
	CreatedAt        string   `json:"createdAt"`
	Tags             []string `json:"tags"`
	IsVisible        bool     `json:"isVisible"`
	URL              string   `json:"url"`
	PageURL          string   `json:"pageUrl"`
	ThumbURL         string   `json:"thumbUrl"`
}

type imagesMetaResponse struct {
	OK    bool            `json:"ok"`
	Image imagesMetaImage `json:"image"`
}

func (a *App) imagesHandleMeta(w http.ResponseWriter, r *http.Request) {
	imageID := r.PathValue("id")
	record, err := a.imagesLoadVisibleRecord(r, imageID)
	if err != nil {
		imagesFail(w, err)
		return
	}

	// 单连接驱动：标签查询在行扫描完成后单独执行，不嵌套结果集。
	tags, err := imagesLoadTagsFor(r.Context(), a.db, []string{imageID})
	if err != nil {
		imagesFail(w, err)
		return
	}
	imageTags := tags[imageID]
	if imageTags == nil {
		imageTags = []string{}
	}

	original, page, thumb := a.imagesURLs(imageID)
	writeJSON(w, http.StatusOK, imagesMetaResponse{
		OK: true,
		Image: imagesMetaImage{
			ID:               imageID,
			OriginalName:     record.OriginalName,
			OriginalMimeType: record.OriginalMimeType,
			Size:             record.Size,
			Width:            record.Width,
			Height:           record.Height,
			CreatedAt:        record.CreatedAt,
			Tags:             imageTags,
			IsVisible:        record.IsVisible,
			URL:              original,
			PageURL:          page,
			ThumbURL:         thumb,
		},
	})
}

// imagesMetaRecord 是详情页可见性判断所需的图片字段。页面路由只需要这份记录，
// 标签与对外 URL 只在 meta handler 里补充，避免页面请求额外查两次库。
type imagesMetaRecord struct {
	OriginalName     string
	OriginalMimeType string
	Size             int64
	Width            int64
	Height           int64
	CreatedAt        string
	IsVisible        bool
}

// imagesLoadVisibleRecord 是 /api/images/{id}/meta 与 /images/{id} 页面路由共用的
// 唯一可见性判断：
//   - 记录不存在、回收站中的记录、管理员下架的记录 → *imagesError 404；
//   - 隐藏图片只认上传者本人的 Cookie（API Key 不算）→ 其他调用者 404；
//   - 其他读取失败原样返回，由调用方决定如何上报（meta 是 500 JSON，页面是 500 壳）。
//
// 页面路由据此决定初始文档状态，不再另写一套判断。
func (a *App) imagesLoadVisibleRecord(r *http.Request, imageID string) (imagesMetaRecord, error) {
	var (
		record         imagesMetaRecord
		uploaderID     int64
		deletedAt      *string
		deletedByAdmin int64
		isVisible      int
	)
	err := a.db.QueryRowContext(r.Context(),
		`SELECT uploaderId, originalName, originalMimeType, size, width, height, createdAt,
			deletedAt, deletedByAdmin, isVisible
		FROM "Image" WHERE id = ?`, imageID).
		Scan(&uploaderID, &record.OriginalName, &record.OriginalMimeType, &record.Size,
			&record.Width, &record.Height, &record.CreatedAt,
			&deletedAt, &deletedByAdmin, &isVisible)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return imagesMetaRecord{}, imagesMetaNotFound()
	case err != nil:
		return imagesMetaRecord{}, err
	}
	if deletedAt != nil || deletedByAdmin != 0 {
		// 回收站与管理端下架的记录对详情页一律不可见；恢复后自然重新可见。
		return imagesMetaRecord{}, imagesMetaNotFound()
	}

	if isVisible == 0 {
		// 用丢弃型 response writer 认证：401 与 404 的差异本身会泄露隐藏图片的存在性。
		user, ok := a.authenticate(imagesDiscardResponse{}, r, false)
		if !ok || user == nil || user.ID != uploaderID {
			return imagesMetaRecord{}, imagesMetaNotFound()
		}
	}

	record.IsVisible = isVisible != 0
	return record, nil
}

// imagesMetaNotFound 是详情不可见时的统一错误：与直接写 404 时的 JSON 信封一致，
// 页面路由通过 errors.As 取出状态，不重复描述"不可见"的原因。
func imagesMetaNotFound() error {
	return &imagesError{status: http.StatusNotFound, code: "NOT_FOUND", message: "Not found"}
}
