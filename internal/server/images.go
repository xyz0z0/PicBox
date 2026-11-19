package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Image API limits. The upload limits mirror the original implementation and
// are deliberately small: this phase runs on a preview data directory and must not
// queue unbounded upload work.
const (
	imagesDefaultUploadMaxBytes int64 = 5 << 20
	imagesDefaultMaxPixels      int64 = 16_000_000
	imagesDefaultListLimit            = 20
	imagesMaxListLimit                = 50
	imagesMaxTags                     = 20
	imagesMaxTagLength                = 64
	imagesMaxRequestJSON              = 64 << 10
	imagesMaxFieldBytes               = 8192
	imagesMaxFields                   = 10
	imagesMaxParts                    = 12
	imagesMaxFiles                    = 1
	imagesMultipartSlack        int64 = 256 << 10
	imagesTempDirName                 = "tmp"
	imagesTimeLayout                  = "2006-01-02T15:04:05.000Z"
	imagesTagMessage                  = "每张图片最多 20 个标签，每个最多 64 个字，且不能包含控制字符"
)

// imagesError carries an HTTP status and the shared error envelope fields.
type imagesError struct {
	status  int
	code    string
	message string
}

func (e *imagesError) Error() string { return e.code + ": " + e.message }

func imagesFail(w http.ResponseWriter, err error) {
	var requestErr *imagesError
	if errors.As(err, &requestErr) {
		writeError(w, requestErr.status, requestErr.code, requestErr.message)
		return
	}
	log.Printf("images: request failed: %v", err)
	writeError(w, http.StatusInternalServerError, "IMAGE_REQUEST_FAILED", "图片操作失败，请稍后重试")
}

// registerImageRoutes wires the image API. The prefix handler keeps unimplemented
// sub-resources explicit: this phase never reports success for work it does not do.
func (a *App) registerImageRoutes() {
	a.mux.HandleFunc("POST /api/images", a.imagesHandleUpload)
	a.mux.HandleFunc("GET /api/images", a.imagesHandleList)
	a.mux.HandleFunc("GET /api/images/{id}", a.imagesHandleFile)
	// 不要额外注册 "HEAD /api/images/{id}"：ServeMux 的 GET 模式已经匹配 HEAD，
	// imagesHandleFile 经 http.ServeContent 也已区分 HEAD（不写 body）。单独注册
	// HEAD 会在方法集上与字面量 GET 子路径（"GET /api/images/tags" 等）冲突，
	// 让 ServeMux 在注册阶段 panic。
	a.mux.HandleFunc("PATCH /api/images/{id}", a.imagesHandlePatch)
	a.mux.HandleFunc("DELETE /api/images/{id}", a.imagesHandleDelete)
	a.mux.HandleFunc("POST /api/images/{id}/restore", a.imagesHandleRestore)
	a.mux.HandleFunc("/api/images/", a.imagesHandleUnsupported)
}

func (a *App) imagesMaxUploadBytes() int64 {
	if a.cfg.UploadMaxBytes > 0 {
		return a.cfg.UploadMaxBytes
	}
	return imagesDefaultUploadMaxBytes
}

func (a *App) imagesMaxPixels() int64 {
	if a.cfg.MaxImagePixels > 0 {
		return a.cfg.MaxImagePixels
	}
	return imagesDefaultMaxPixels
}

// imagesTryLockUpload takes the single upload processing slot without blocking.
// A busy slot is reported as 429 instead of building a wait queue.
func (a *App) imagesTryLockUpload() (func(), bool) {
	slots := a.uploadSlots
	if slots == nil {
		// Without a configured slot (core did not initialize it) uploads still run,
		// but they are not serialized.
		return func() {}, true
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	default:
		return nil, false
	}
}

func imagesTruthy(value string) bool {
	return value == "true" || value == "1"
}

func imagesBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func imagesTagError() *imagesError {
	return &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG", message: imagesTagMessage}
}

// imagesNormalizeTags trims, de-duplicates, validates and sorts tags. Empty tags
// are dropped (legacy single-tag uploads may send an empty string).
func imagesNormalizeTags(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	tags := make([]string, 0, len(values))
	for _, value := range values {
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return nil, imagesTagError()
			}
		}
		tag := strings.TrimSpace(value)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > imagesMaxTagLength {
			return nil, imagesTagError()
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
		if len(tags) > imagesMaxTags {
			return nil, imagesTagError()
		}
	}
	sort.Strings(tags)
	return tags, nil
}

// imagesTagsFromFields parses the multipart tag fields. The legacy scalar `tag`
// and the array `tags` must never be combined.
func imagesTagsFromFields(fields map[string]string) ([]string, error) {
	tagsValue, hasTags := fields["tags"]
	tagValue, hasTag := fields["tag"]
	if hasTag && hasTags {
		return nil, imagesTagError()
	}
	if hasTags {
		return imagesNormalizeTagsJSON(tagsValue)
	}
	if hasTag {
		return imagesNormalizeTags([]string{tagValue})
	}
	return []string{}, nil
}

func imagesNormalizeTagsJSON(raw string) ([]string, error) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG", message: "tags 必须是 JSON 字符串数组"}
	}
	entries, ok := decoded.([]any)
	if !ok {
		return nil, imagesTagError()
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		value, ok := entry.(string)
		if !ok {
			return nil, imagesTagError()
		}
		values = append(values, value)
	}
	return imagesNormalizeTags(values)
}

// imagesURLs builds the public URLs for an image id using the configured BaseURL.
func (a *App) imagesURLs(id string) (original, page, thumb string) {
	base := strings.TrimRight(a.cfg.BaseURL, "/")
	return base + "/api/images/" + id + "?variant=original",
		base + "/images/" + id,
		base + "/api/images/" + id + "?variant=thumb"
}

type imagesUploadResponse struct {
	OK       bool     `json:"ok"`
	ID       string   `json:"id"`
	Tag      *string  `json:"tag"`
	Tags     []string `json:"tags"`
	URL      string   `json:"url"`
	PageURL  string   `json:"pageUrl"`
	ThumbURL string   `json:"thumbUrl"`
}

func (a *App) imagesHandleUpload(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	// 与 URL 导入共用同一个上传频率桶（core 提供，按可信 clientIP 100 次/小时）。
	// 放在读取/解码与单处理槽之前：超限请求不落临时文件、不解码，也不占用上传槽；
	// 单槽的 UPLOAD_BUSY 仍是独立的并发背压，两者互不替代。
	if !a.limitImageUpload(w, r) {
		return
	}
	release, ok := a.imagesTryLockUpload()
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "UPLOAD_BUSY", "已有图片正在处理，请稍后重试")
		return
	}
	defer release()

	upload, err := a.imagesReadUpload(w, r)
	if err != nil {
		imagesFail(w, err)
		return
	}
	defer upload.cleanup()

	response, err := a.imagesStoreUpload(r.Context(), user.ID, upload)
	if err != nil {
		imagesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type imagesListItem struct {
	ID               string   `json:"id"`
	OriginalName     string   `json:"originalName"`
	Tag              *string  `json:"tag"`
	Tags             []string `json:"tags"`
	CreatedAt        string   `json:"createdAt"`
	IsVisible        bool     `json:"isVisible"`
	DeletedAt        *string  `json:"deletedAt"`
	Width            *int64   `json:"width"`
	Height           *int64   `json:"height"`
	Size             int64    `json:"size"`
	OriginalMimeType string   `json:"originalMimeType"`
	URL              string   `json:"url"`
	PageURL          string   `json:"pageUrl"`
	ThumbURL         string   `json:"thumbUrl"`
}

type imagesListResponse struct {
	OK         bool             `json:"ok"`
	Items      []imagesListItem `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	Total      int64            `json:"total"`
}

type imagesListParams struct {
	mine       bool
	deleted    bool
	visibility string
	limit      int
	cursor     string
	tags       []string
	tagMatch   string
	query      string
	sort       string
}

func imagesParseListParams(q url.Values) (imagesListParams, error) {
	params := imagesListParams{
		mine: imagesTruthy(q.Get("mine")),
		// `deleted=true` is the recycle bin switch. `trashed` is the first-phase
		// spelling of the same filter and stays accepted for existing preview clients.
		deleted:    imagesTruthy(q.Get("deleted")) || imagesTruthy(q.Get("trashed")),
		visibility: strings.TrimSpace(q.Get("visibility")),
		cursor:     strings.TrimSpace(q.Get("cursor")),
		tagMatch:   strings.TrimSpace(q.Get("tagMatch")),
		query:      strings.TrimSpace(q.Get("query")),
		sort:       strings.TrimSpace(q.Get("sort")),
	}
	if params.visibility == "" {
		params.visibility = "all"
	}
	if params.tagMatch == "" {
		params.tagMatch = "any"
	}
	if params.sort == "" {
		params.sort = "newest"
	}
	switch params.sort {
	case "newest", "oldest", "name", "size":
	default:
		return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_SORT", message: "sort must be newest, oldest, name, or size"}
	}
	switch params.visibility {
	case "all", "visible", "hidden":
	default:
		return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_VISIBILITY", message: "visibility must be all, visible, or hidden"}
	}
	if params.tagMatch != "any" && params.tagMatch != "all" {
		return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG_MATCH", message: "tagMatch must be any or all"}
	}

	params.limit = imagesDefaultListLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			params.limit = parsed
		}
	}
	if params.limit < 1 {
		params.limit = 1
	}
	if params.limit > imagesMaxListLimit {
		params.limit = imagesMaxListLimit
	}

	if params.deleted && !params.mine {
		// The recycle bin is per-account: it must never be reachable from the public
		// list or through an API key.
		return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "deleted requires mine=true"}
	}
	rawTags := q["tag"]
	if len(rawTags) > 0 {
		// Tag filters only make sense on the personal list; the public list has no
		// tag filter in the existing API, so reject instead of silently ignoring.
		if !params.mine {
			return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG", message: "tag filtering requires mine=true"}
		}
		for _, raw := range rawTags {
			if strings.TrimSpace(raw) == "" {
				return params, &imagesError{status: http.StatusBadRequest, code: "INVALID_TAG", message: "Invalid tag"}
			}
		}
		tags, err := imagesNormalizeTags(rawTags)
		if err != nil {
			return params, err
		}
		params.tags = tags
	}
	return params, nil
}

// whereClause builds the shared filter. Cursor conditions are appended by the
// caller because they depend on the sort mode.
func (p imagesListParams) whereClause(userID int64) (string, []any) {
	conds := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if p.mine {
		conds = append(conds, "uploaderId = ?")
		args = append(args, userID)
		if p.deleted {
			// Recycle bin: only the uploader's own soft deletions, never images
			// removed by an administrator (those are not the uploader's to restore).
			conds = append(conds, "deletedAt IS NOT NULL", "deletedByAdmin = 0")
		} else {
			conds = append(conds, "deletedAt IS NULL")
			switch p.visibility {
			case "visible":
				conds = append(conds, "isVisible = 1")
			case "hidden":
				conds = append(conds, "isVisible = 0")
			}
		}
	} else {
		conds = append(conds, "deletedAt IS NULL")
		// The public list never shows hidden images. Asking for hidden explicitly
		// yields an empty page instead of exposing hidden rows or silently ignoring
		// the parameter.
		if p.visibility == "hidden" {
			conds = append(conds, "1 = 0")
		} else {
			conds = append(conds, "isVisible = 1")
		}
	}
	if p.query != "" {
		conds = append(conds, `originalName LIKE ? ESCAPE '\'`)
		args = append(args, "%"+imagesLikeEscape(p.query)+"%")
	}
	if len(p.tags) > 0 {
		if p.tagMatch == "all" {
			for _, tag := range p.tags {
				conds = append(conds, "id IN (SELECT imageId FROM ImageTag WHERE name = ?)")
				args = append(args, tag)
			}
		} else {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(p.tags)), ",")
			conds = append(conds, "id IN (SELECT imageId FROM ImageTag WHERE name IN ("+placeholders+"))")
			for _, tag := range p.tags {
				args = append(args, tag)
			}
		}
	}
	return strings.Join(conds, " AND "), args
}

func imagesLikeEscape(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func (a *App) imagesHandleList(w http.ResponseWriter, r *http.Request) {
	params, err := imagesParseListParams(r.URL.Query())
	if err != nil {
		imagesFail(w, err)
		return
	}

	var user *User
	if params.mine {
		authenticated, ok := a.authenticate(w, r, false)
		if !ok {
			return
		}
		if authenticated == nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
			return
		}
		user = authenticated
	}
	var userID int64
	if user != nil {
		userID = user.ID
	}

	whereSQL, whereArgs := params.whereClause(userID)
	total := int64(0)
	if err := a.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM Image WHERE "+whereSQL, whereArgs...).Scan(&total); err != nil {
		imagesFail(w, err)
		return
	}

	queryWhere := " WHERE " + whereSQL
	args := append([]any{}, whereArgs...)
	offset := 0
	if params.sort == "newest" || params.sort == "oldest" {
		if params.cursor != "" {
			if cursorTime, cursorID, ok := imagesParseDateCursor(params.cursor); ok {
				if cursorID != "" {
					if params.sort == "newest" {
						queryWhere += " AND (createdAt < ? OR (createdAt = ? AND id < ?))"
					} else {
						queryWhere += " AND (createdAt > ? OR (createdAt = ? AND id > ?))"
					}
					args = append(args, cursorTime, cursorTime, cursorID)
				} else if params.sort == "newest" {
					queryWhere += " AND createdAt < ?"
					args = append(args, cursorTime)
				} else {
					queryWhere += " AND createdAt > ?"
					args = append(args, cursorTime)
				}
			}
		}
	} else if params.cursor != "" && strings.HasPrefix(params.cursor, "o:") {
		if parsed, err := strconv.Atoi(params.cursor[2:]); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	orderSQL := "createdAt DESC, id DESC"
	switch params.sort {
	case "oldest":
		orderSQL = "createdAt ASC, id ASC"
	case "name":
		orderSQL = "originalName ASC, id ASC"
	case "size":
		orderSQL = "size DESC, id ASC"
	}

	statement := "SELECT id, originalName, size, width, height, originalMimeType, createdAt, deletedAt, isVisible FROM Image" +
		queryWhere + " ORDER BY " + orderSQL + " LIMIT ?"
	args = append(args, params.limit)
	if params.sort == "name" || params.sort == "size" {
		statement += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := a.db.QueryContext(r.Context(), statement, args...)
	if err != nil {
		imagesFail(w, err)
		return
	}
	items := make([]imagesListItem, 0, params.limit)
	ids := make([]string, 0, params.limit)
	for rows.Next() {
		var (
			id           string
			originalName string
			size         int64
			width        *int64
			height       *int64
			mimeType     string
			createdAt    string
			deletedAt    *string
			isVisible    int
		)
		if err := rows.Scan(&id, &originalName, &size, &width, &height, &mimeType, &createdAt, &deletedAt, &isVisible); err != nil {
			rows.Close()
			imagesFail(w, err)
			return
		}
		original, page, thumb := a.imagesURLs(id)
		items = append(items, imagesListItem{
			ID:               id,
			OriginalName:     originalName,
			Tags:             []string{},
			CreatedAt:        createdAt,
			IsVisible:        isVisible != 0,
			DeletedAt:        deletedAt,
			Width:            width,
			Height:           height,
			Size:             size,
			OriginalMimeType: mimeType,
			URL:              original,
			PageURL:          page,
			ThumbURL:         thumb,
		})
		ids = append(ids, id)
	}
	scanErr := rows.Err()
	// Close before the tag query: the driver uses a single connection, so an open
	// result set would deadlock any nested query.
	rows.Close()
	if scanErr != nil {
		imagesFail(w, scanErr)
		return
	}

	tagsByImage := make(map[string][]string, len(ids))
	if len(ids) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		tagArgs := make([]any, 0, len(ids))
		for _, id := range ids {
			tagArgs = append(tagArgs, id)
		}
		tagRows, err := a.db.QueryContext(r.Context(),
			"SELECT imageId, name FROM ImageTag WHERE imageId IN ("+placeholders+") ORDER BY imageId, name", tagArgs...)
		if err != nil {
			imagesFail(w, err)
			return
		}
		for tagRows.Next() {
			var imageID, name string
			if err := tagRows.Scan(&imageID, &name); err != nil {
				tagRows.Close()
				imagesFail(w, err)
				return
			}
			tagsByImage[imageID] = append(tagsByImage[imageID], name)
		}
		tagErr := tagRows.Err()
		tagRows.Close()
		if tagErr != nil {
			imagesFail(w, tagErr)
			return
		}
	}
	for index := range items {
		tags := tagsByImage[items[index].ID]
		if tags == nil {
			tags = []string{}
		}
		items[index].Tags = tags
		if len(tags) > 0 {
			first := tags[0]
			items[index].Tag = &first
		}
	}

	var nextCursor *string
	if len(items) == params.limit && len(items) > 0 {
		var cursor string
		if params.sort == "name" || params.sort == "size" {
			cursor = "o:" + strconv.Itoa(offset+len(items))
		} else {
			last := items[len(items)-1]
			cursor = last.CreatedAt + "|" + last.ID
		}
		nextCursor = &cursor
	}

	writeJSON(w, http.StatusOK, imagesListResponse{
		OK:         true,
		Items:      items,
		NextCursor: nextCursor,
		Total:      total,
	})
}

type imagesPatchImage struct {
	ID        string   `json:"id"`
	IsVisible bool     `json:"isVisible"`
	Tag       *string  `json:"tag"`
	Tags      []string `json:"tags"`
}

type imagesPatchResponse struct {
	OK    bool             `json:"ok"`
	Image imagesPatchImage `json:"image"`
}

func (a *App) imagesHandlePatch(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authenticate(w, r, true)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, imagesMaxRequestJSON+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected a JSON object")
		return
	}
	if int64(len(body)) > imagesMaxRequestJSON {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected a JSON object")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected a JSON object")
		return
	}

	hasVisible := false
	visible := false
	if value, ok := raw["visible"]; ok {
		// json.Unmarshal(null, &bool) succeeds and leaves false, so an explicit null
		// must be rejected: it is not a boolean and would silently hide the image.
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Provide tags and/or a boolean visible")
			return
		}
		if err := json.Unmarshal(value, &visible); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Provide tags and/or a boolean visible")
			return
		}
		hasVisible = true
	}
	hasTags := false
	tags := []string{}
	if value, ok := raw["tags"]; ok {
		var incoming []string
		// An explicit JSON null is not an empty tag list: replacing tags with null
		// would silently clear them, so reject it like the existing API does.
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			imagesFail(w, imagesTagError())
			return
		}
		if err := json.Unmarshal(value, &incoming); err != nil {
			imagesFail(w, imagesTagError())
			return
		}
		normalized, err := imagesNormalizeTags(incoming)
		if err != nil {
			imagesFail(w, err)
			return
		}
		tags = normalized
		hasTags = true
	}
	if !hasVisible && !hasTags {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Provide tags and/or a boolean visible")
		return
	}

	imageID := r.PathValue("id")
	now := imagesNow()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		imagesFail(w, err)
		return
	}
	defer tx.Rollback()

	var uploaderID int64
	var deletedAt *string
	err = tx.QueryRowContext(r.Context(), "SELECT uploaderId, deletedAt FROM Image WHERE id = ?", imageID).Scan(&uploaderID, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedAt != nil) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if err != nil {
		imagesFail(w, err)
		return
	}
	if uploaderID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Forbidden")
		return
	}

	if hasVisible {
		if _, err := tx.ExecContext(r.Context(), "UPDATE Image SET isVisible = ?, updatedAt = ? WHERE id = ?", imagesBoolInt(visible), now, imageID); err != nil {
			imagesFail(w, err)
			return
		}
	} else if _, err := tx.ExecContext(r.Context(), "UPDATE Image SET updatedAt = ? WHERE id = ?", now, imageID); err != nil {
		imagesFail(w, err)
		return
	}
	if hasTags {
		if _, err := tx.ExecContext(r.Context(), "DELETE FROM ImageTag WHERE imageId = ?", imageID); err != nil {
			imagesFail(w, err)
			return
		}
		for _, name := range tags {
			if _, err := tx.ExecContext(r.Context(), "INSERT INTO ImageTag (imageId, name) VALUES (?, ?)", imageID, name); err != nil {
				imagesFail(w, err)
				return
			}
		}
	}

	var currentVisible int
	if err := tx.QueryRowContext(r.Context(), "SELECT isVisible FROM Image WHERE id = ?", imageID).Scan(&currentVisible); err != nil {
		imagesFail(w, err)
		return
	}
	currentTags := make([]string, 0, len(tags))
	tagRows, err := tx.QueryContext(r.Context(), "SELECT name FROM ImageTag WHERE imageId = ? ORDER BY name", imageID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	for tagRows.Next() {
		var name string
		if err := tagRows.Scan(&name); err != nil {
			tagRows.Close()
			imagesFail(w, err)
			return
		}
		currentTags = append(currentTags, name)
	}
	tagScanErr := tagRows.Err()
	tagRows.Close()
	if tagScanErr != nil {
		imagesFail(w, tagScanErr)
		return
	}
	if err := tx.Commit(); err != nil {
		imagesFail(w, err)
		return
	}

	image := imagesPatchImage{ID: imageID, IsVisible: currentVisible != 0, Tags: currentTags}
	if len(currentTags) > 0 {
		first := currentTags[0]
		image.Tag = &first
	}
	writeJSON(w, http.StatusOK, imagesPatchResponse{OK: true, Image: image})
}

func (a *App) imagesHandleDelete(w http.ResponseWriter, r *http.Request) {
	permanent := imagesTruthy(r.URL.Query().Get("permanent"))
	// Permanent deletion removes bytes from disk, so it only accepts the owner's login
	// Cookie. An API key may still soft-delete (the existing contract), never purge.
	user, ok := a.authenticate(w, r, !permanent)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if permanent {
		a.imagesHandlePurge(w, r, user)
		return
	}

	imageID := r.PathValue("id")
	var uploaderID int64
	var deletedAt *string
	err := a.db.QueryRowContext(r.Context(), "SELECT uploaderId, deletedAt FROM Image WHERE id = ?", imageID).Scan(&uploaderID, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if err != nil {
		imagesFail(w, err)
		return
	}
	if uploaderID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Forbidden")
		return
	}
	if deletedAt != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}

	now := imagesNow()
	result, err := a.db.ExecContext(r.Context(),
		"UPDATE Image SET deletedAt = ?, updatedAt = ? WHERE id = ? AND deletedAt IS NULL", now, now, imageID)
	if err != nil {
		imagesFail(w, err)
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		imagesFail(w, err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// imagesHandleUnsupported answers sub-resources that this phase does not implement.
// Unknown paths stay 404; known but unimplemented features are 501 so clients can
// tell "no such route" from "not built yet".
func (a *App) imagesHandleUnsupported(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/images/")
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	switch segments[0] {
	case "batch", "export", "import", "url", "trash", "migrate":
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "该图片功能尚未实现")
		return
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
}

// imagesParseDateCursor accepts both the composite `createdAt|id` cursor emitted by
// this implementation and a bare RFC3339 timestamp (the previous cursor format).
func imagesParseDateCursor(value string) (stored string, id string, ok bool) {
	if index := strings.Index(value, "|"); index > 0 {
		parsed, ok := imagesParseTime(value[:index])
		if !ok {
			return "", "", false
		}
		return parsed.Format(imagesTimeLayout), value[index+1:], true
	}
	parsed, ok := imagesParseTime(value)
	if !ok {
		return "", "", false
	}
	return parsed.Format(imagesTimeLayout), "", true
}

func imagesParseTime(value string) (time.Time, bool) {
	for _, layout := range []string{imagesTimeLayout, time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func imagesNow() string {
	return time.Now().UTC().Truncate(time.Millisecond).Format(imagesTimeLayout)
}
