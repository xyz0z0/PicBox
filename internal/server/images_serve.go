package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	imagesVariantOriginal = "original"
	imagesVariantThumb    = "thumb"
)

type imagesServeRow struct {
	UploaderID       int64
	DeletedAt        sql.NullString
	DeletedByAdmin   int64
	IsVisible        int64
	OriginalPath     string
	ThumbPath        string
	OriginalMimeType string
	SHA256           sql.NullString
	Size             int64
}

// imagesDiscardResponse lets a handler authenticate without writing a response.
// Deleted images must look like 404 to everyone who is not the uploader, so a
// failed authentication must not leak 401 vs 404 differences.
type imagesDiscardResponse struct{}

func (imagesDiscardResponse) Header() http.Header            { return http.Header{} }
func (imagesDiscardResponse) Write(body []byte) (int, error) { return len(body), nil }
func (imagesDiscardResponse) WriteHeader(statusCode int)     {}

func (a *App) imagesHandleFile(w http.ResponseWriter, r *http.Request) {
	variant := imagesVariantOriginal
	if r.URL.Query().Get("variant") == imagesVariantThumb {
		variant = imagesVariantThumb
	}

	imageID := r.PathValue("id")
	var row imagesServeRow
	err := a.db.QueryRowContext(r.Context(),
		`SELECT uploaderId, deletedAt, deletedByAdmin, isVisible, originalPath, thumbPath, originalMimeType, sha256, size
		FROM Image WHERE id = ?`, imageID).
		Scan(&row.UploaderID, &row.DeletedAt, &row.DeletedByAdmin, &row.IsVisible,
			&row.OriginalPath, &row.ThumbPath, &row.OriginalMimeType, &row.SHA256, &row.Size)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if err != nil {
		imagesFail(w, err)
		return
	}

	// Authorization and visibility are decided before any conditional request is
	// evaluated, so a 304 can never confirm the existence of hidden or deleted
	// content.
	private := false
	if row.DeletedAt.Valid {
		// Trashed images: only the uploader's own session cookie. API keys are
		// rejected, and admin removals stay invisible to the uploader.
		user, ok := a.authenticate(imagesDiscardResponse{}, r, false)
		if !ok || user == nil || row.DeletedByAdmin != 0 || user.ID != row.UploaderID {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
			return
		}
		private = true
	}

	if row.IsVisible == 0 {
		a.imagesWritePlaceholder(w, r, variant)
		return
	}

	relative := row.OriginalPath
	if variant == imagesVariantThumb {
		relative = row.ThumbPath
	}
	absolute, err := a.imagesSafePath(relative)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}

	etag := imagesETag(row, variant, relative, info)
	cacheControl := "public, no-cache"
	if private {
		// Owner-only responses must never enter a shared cache.
		cacheControl = "private, no-store"
	}
	w.Header().Set("Content-Type", imagesContentType(relative, row.OriginalMimeType, variant))
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")

	serveRequest := r
	if private {
		// A trashed image must be re-validated on every request; suppress cache
		// validators so the owner also gets fresh bytes.
		cloned := r.Clone(r.Context())
		cloned.Header.Del("If-None-Match")
		cloned.Header.Del("If-Modified-Since")
		serveRequest = cloned
	}

	file, err := os.Open(absolute)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	defer file.Close()
	// GET and HEAD share one path: ServeContent handles ETag/If-None-Match (304),
	// If-Modified-Since, single ranges (206/416) and suppresses the body for HEAD.
	// A separate HEAD shortcut here previously returned 200 even when the GET
	// equivalent was 304, which is a different conditional contract.
	http.ServeContent(w, serveRequest, "", info.ModTime(), file)
}

func imagesETag(row imagesServeRow, variant, relative string, info os.FileInfo) string {
	if row.SHA256.Valid && row.SHA256.String != "" {
		if variant == imagesVariantThumb {
			return `"` + row.SHA256.String + `-thumb"`
		}
		return `"` + row.SHA256.String + `"`
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%s", relative, info.Size(), info.ModTime().UnixNano(), variant)))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// imagesContentType maps the persisted extension of the stored file to the real
// MIME type. The original is never re-encoded, so the extension is the format.
func imagesContentType(relative, originalMime, variant string) string {
	if variant == imagesVariantThumb {
		return "image/webp"
	}
	switch strings.ToLower(path.Ext(relative)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	switch originalMime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return originalMime
	}
	return "application/octet-stream"
}

// imagesWritePlaceholder serves the fixed SVG used for hidden images. It is never
// cached and cannot be mistaken for the real bytes.
func (a *App) imagesWritePlaceholder(w http.ResponseWriter, r *http.Request, variant string) {
	body := []byte(imagesPlaceholderSVG(variant))
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func imagesPlaceholderSVG(variant string) string {
	width, height := 1280, 720
	if variant == imagesVariantThumb {
		width, height = 400, 300
	}
	// Fixed markup with no user-controlled values.
	return `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="` + strconv.Itoa(width) + `" height="` + strconv.Itoa(height) + `" viewBox="0 0 ` + strconv.Itoa(width) + ` ` + strconv.Itoa(height) + `">
  <rect width="100%" height="100%" fill="#f3f4f6" />
  <g fill="#6b7280" text-anchor="middle" font-family="Arial, sans-serif">
    <text x="50%" y="46%" font-size="28">图片暂不可见</text>
    <text x="50%" y="56%" font-size="16">This image is hidden by uploader</text>
  </g>
</svg>`
}
