package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	stddraw "image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// 上传频率桶沿用原版 lib/rateLimit.ts 的 limiterUpload：100 次 / 小时 / 客户端 IP。
// 它只限制请求总量，不代替单处理槽的并发背压（满载仍返回 429 UPLOAD_BUSY）。
const (
	uploadImageRateLimit  = 100
	uploadImageRateWindow = time.Hour
)

// limitImageUpload 按客户端 IP 给图片上传（multipart 与 URL 导入共用一个桶）分桶。
// 被限流时直接写出 429 RATE_LIMITED + Retry-After（与其它限流接口同一个响应封装）
// 并返回 false，调用方应立即返回：不读请求体、不创建临时文件、不解码。
func (a *App) limitImageUpload(w http.ResponseWriter, r *http.Request) bool {
	if a.uploadLimiter == nil {
		// core 未接线时上传依旧可用（与 imagesTryLockUpload 的兼容行为一致）。
		return true
	}
	if allowed, retryAfter := a.uploadLimiter.allow("ip:" + a.clientIP(r)); !allowed {
		writeRateLimited(w, retryAfter)
		return false
	}
	return true
}

// imagesUpload is the streaming result of the multipart request. The file is kept
// in data/tmp until the stored upload succeeds; every failure path removes it.
type imagesUpload struct {
	tempPath string
	name     string
	size     int64
	sha256   string
	tags     []string
}

func (u *imagesUpload) cleanup() {
	if u.tempPath == "" {
		return
	}
	if err := os.Remove(u.tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		logImagesCleanup("staged upload", u.tempPath, err)
	}
	u.tempPath = ""
}

func logImagesCleanup(kind, target string, err error) {
	// Cleanup failures leave files behind; say so instead of hiding them.
	log.Printf("images: failed to clean up %s %s: %v", kind, target, err)
}

// imagesReadUpload streams the multipart body to a temp file. It never buffers the
// whole image: the file limit, field limits and the total request size are enforced
// while reading.
func (a *App) imagesReadUpload(w http.ResponseWriter, r *http.Request) (*imagesUpload, error) {
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return nil, &imagesError{status: http.StatusInternalServerError, code: "IMAGE_REQUEST_FAILED", message: "服务数据目录未配置"}
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "Expected multipart/form-data"}
	}
	maxFile := a.imagesMaxUploadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, maxFile+imagesMultipartSlack)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "无效的上传表单"}
	}

	tempDir := filepath.Join(a.cfg.DataDir, imagesTempDirName)
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		return nil, fmt.Errorf("create upload temp dir: %w", err)
	}
	tempFile, err := os.CreateTemp(tempDir, "upload-*.part")
	if err != nil {
		return nil, fmt.Errorf("create upload temp file: %w", err)
	}
	tempPath := tempFile.Name()
	discard := func() {
		tempFile.Close()
		os.Remove(tempPath)
	}

	hasher := sha256.New()
	var size int64
	name := "upload"
	fields := make(map[string]string, 2)
	parts, files, fieldCount := 0, 0, 0

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			discard()
			if imagesBodyTooLarge(err) {
				return nil, &imagesError{status: http.StatusRequestEntityTooLarge, code: "FILE_TOO_LARGE", message: "图片超过上传大小限制"}
			}
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传中断或表单不完整"}
		}
		parts++
		if parts > imagesMaxParts {
			part.Close()
			discard()
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传表单字段过多"}
		}
		partName := part.FormName()
		filename := part.FileName()
		if partName == "file" || filename != "" {
			files++
			if files > imagesMaxFiles || partName != "file" {
				part.Close()
				discard()
				return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "只允许上传一个图片文件"}
			}
			if filename == "" {
				part.Close()
				discard()
				return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "请选择图片文件"}
			}
			name = imagesCleanFilename(filename)
			written, copyErr := io.Copy(io.MultiWriter(tempFile, hasher), io.LimitReader(part, maxFile+1))
			part.Close()
			if copyErr != nil {
				discard()
				if imagesBodyTooLarge(copyErr) {
					return nil, &imagesError{status: http.StatusRequestEntityTooLarge, code: "FILE_TOO_LARGE", message: "图片超过上传大小限制"}
				}
				return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传中断或表单不完整"}
			}
			if written > maxFile {
				discard()
				return nil, &imagesError{
					status:  http.StatusRequestEntityTooLarge,
					code:    "FILE_TOO_LARGE",
					message: fmt.Sprintf("图片超过上传大小限制（%d 字节）", maxFile),
				}
			}
			size = written
			continue
		}

		fieldCount++
		if fieldCount > imagesMaxFields {
			part.Close()
			discard()
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传字段过多"}
		}
		value, readErr := io.ReadAll(io.LimitReader(part, imagesMaxFieldBytes+1))
		part.Close()
		if readErr != nil {
			discard()
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传中断或表单不完整"}
		}
		if len(value) > imagesMaxFieldBytes {
			discard()
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传字段超过长度限制"}
		}
		if partName != "tag" && partName != "tags" {
			continue
		}
		if _, exists := fields[partName]; exists {
			discard()
			return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "上传字段重复"}
		}
		fields[partName] = string(value)
	}

	if files == 0 || size == 0 {
		discard()
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_FILE", message: "请选择图片文件"}
	}
	tags, err := imagesTagsFromFields(fields)
	if err != nil {
		discard()
		return nil, err
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return nil, fmt.Errorf("close staged upload: %w", err)
	}
	return &imagesUpload{
		tempPath: tempPath,
		name:     name,
		size:     size,
		sha256:   hex.EncodeToString(hasher.Sum(nil)),
		tags:     tags,
	}, nil
}

func imagesBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}

// imagesCleanFilename keeps a display name only: no path separators, no control
// characters and a bounded length. The physical path is always generated.
func imagesCleanFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	var builder strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
	}
	cleaned := strings.TrimSpace(builder.String())
	if cleaned == "" || cleaned == "." || cleaned == "/" {
		return "upload"
	}
	runes := []rune(cleaned)
	if len(runes) > 255 {
		cleaned = string(runes[:255])
	}
	return cleaned
}

type imagesFormat struct {
	mime string
	ext  string
}

// imagesDetectFormat reads the real container format from the leading bytes. SVG
// and every other document type are rejected here, before any decoder sees them.
func imagesDetectFormat(head []byte) (imagesFormat, bool) {
	switch {
	case len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return imagesFormat{mime: "image/png", ext: "png"}, true
	case len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff:
		return imagesFormat{mime: "image/jpeg", ext: "jpg"}, true
	case len(head) >= 6 && (bytes.Equal(head[:6], []byte("GIF87a")) || bytes.Equal(head[:6], []byte("GIF89a"))):
		return imagesFormat{mime: "image/gif", ext: "gif"}, true
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return imagesFormat{mime: "image/webp", ext: "webp"}, true
	}
	return imagesFormat{}, false
}

func imagesFormatForGoName(name string) (imagesFormat, bool) {
	switch name {
	case "png":
		return imagesFormat{mime: "image/png", ext: "png"}, true
	case "jpeg":
		return imagesFormat{mime: "image/jpeg", ext: "jpg"}, true
	case "gif":
		return imagesFormat{mime: "image/gif", ext: "gif"}, true
	case "webp":
		return imagesFormat{mime: "image/webp", ext: "webp"}, true
	}
	return imagesFormat{}, false
}

func imagesReadHead(filePath string) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, 32)
	read, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:read], nil
}

// imagesDecodeConfig inspects only the header. It must run before Decode so a
// hostile image cannot force a large allocation.
func imagesDecodeConfig(filePath string) (image.Config, string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return image.Config{}, "", err
	}
	defer file.Close()
	return image.DecodeConfig(file)
}

// imagesCheckDimensions rejects zero/negative sizes and multiplications that would
// overflow. The pixel cap is a preview-resource boundary, not a production default.
func imagesCheckDimensions(width, height int, maxPixels int64) error {
	if width <= 0 || height <= 0 {
		return &imagesError{status: http.StatusBadRequest, code: "INVALID_IMAGE", message: "无法处理该图片，请尝试其它文件"}
	}
	w := int64(width)
	h := int64(height)
	if w > maxPixels || h > maxPixels || w > maxPixels/h {
		return &imagesError{
			status:  http.StatusBadRequest,
			code:    "IMAGE_TOO_LARGE",
			message: fmt.Sprintf("图片像素超过限制（最多 %d 像素）", maxPixels),
		}
	}
	return nil
}

// imagesThumbnail decodes the first frame and encodes a lossless WebP thumbnail.
//
// Only the thumbnail is re-encoded; the original bytes are stored untouched. The
// previous implementation used sharp with quality=80 lossy WebP, so thumbnails are
// now lossless (larger, no generation loss) - this is an intentional difference.
func imagesThumbnail(filePath string, animatedWebP bool, maxPixels int64) ([]byte, error) {
	var (
		source image.Image
		err    error
	)
	if animatedWebP {
		// 动画 WebP 取首帧并合成到 VP8X 画布；后续帧不解码。
		source, err = imagesWebPAnimatedFirstFrame(filePath, maxPixels)
	} else {
		source, err = imagesDecodeStill(filePath)
	}
	if err != nil {
		return nil, err
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("decoded image has empty bounds")
	}
	targetWidth, targetHeight := width, height
	if width > imagesThumbMaxEdge || height > imagesThumbMaxEdge {
		// Shrink to fit a 400x400 box; small images are never enlarged.
		scale := math.Min(float64(imagesThumbMaxEdge)/float64(width), float64(imagesThumbMaxEdge)/float64(height))
		targetWidth = max(1, int(math.Round(float64(width)*scale)))
		targetHeight = max(1, int(math.Round(float64(height)*scale)))
	}
	canvas := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	if targetWidth == width && targetHeight == height {
		stddraw.Draw(canvas, canvas.Bounds(), source, bounds.Min, stddraw.Src)
	} else {
		xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), source, bounds, stddraw.Src, nil)
	}
	var encoded bytes.Buffer
	if err := nativewebp.Encode(&encoded, canvas, nil); err != nil {
		return nil, err
	}
	if encoded.Len() == 0 {
		return nil, fmt.Errorf("empty webp thumbnail")
	}
	return encoded.Bytes(), nil
}

func imagesDecodeStill(filePath string) (image.Image, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	source, _, err := image.Decode(file)
	return source, err
}

// imagesStoragePaths derives service-generated relative paths. Callers never use
// client input here, so a stored path cannot escape the data directory.
func imagesStoragePaths(uploaderID int64, id string, createdAt time.Time, extension string) (original, thumb string) {
	month := createdAt.UTC().Format("2006/01")
	prefix := id
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	directory := "images/" + fmt.Sprintf("%d", uploaderID) + "/" + month + "/" + prefix + "/" + id
	return directory + "/original." + extension, directory + "/thumb.webp"
}

const imagesThumbMaxEdge = 400

// imagesSafePath resolves a stored relative path and refuses anything that leaves
// the configured data directory.
func (a *App) imagesSafePath(relative string) (string, error) {
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return "", fmt.Errorf("data directory is not configured")
	}
	if relative == "" || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("invalid image path %q", relative)
	}
	cleaned := path.Clean(relative)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid image path %q", relative)
	}
	root, err := filepath.Abs(a.cfg.DataDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(cleaned))
	relativeToRoot, err := filepath.Rel(root, target)
	if err != nil || relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image path escapes data directory: %q", relative)
	}
	return target, nil
}

type imagesRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// imagesQuotaBytes resolves the effective quota: the per-user override when set,
// otherwise the configured default. 0 means unlimited.
func imagesQuotaBytes(ctx context.Context, querier imagesRowQuerier, userID int64, fallback int64) (int64, error) {
	var override sql.NullInt64
	err := querier.QueryRowContext(ctx, "SELECT quotaBytes FROM User WHERE id = ?", userID).Scan(&override)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &imagesError{status: http.StatusUnauthorized, code: "UNAUTHORIZED", message: "账号不存在"}
	}
	if err != nil {
		return 0, fmt.Errorf("read user quota: %w", err)
	}
	if override.Valid {
		return override.Int64, nil
	}
	return fallback, nil
}

// imagesUsedBytes counts every stored image, including trashed and admin-hidden
// ones: their files still occupy disk until they are physically removed.
func imagesUsedBytes(ctx context.Context, querier imagesRowQuerier, userID int64) (int64, error) {
	var used sql.NullInt64
	if err := querier.QueryRowContext(ctx, "SELECT SUM(size) FROM Image WHERE uploaderId = ?", userID).Scan(&used); err != nil {
		return 0, fmt.Errorf("read storage usage: %w", err)
	}
	if !used.Valid {
		return 0, nil
	}
	return used.Int64, nil
}

func imagesQuotaError(used, quota int64) error {
	return &imagesError{
		status: http.StatusRequestEntityTooLarge,
		code:   "USER_QUOTA_EXCEEDED",
		message: fmt.Sprintf("存储空间不足：已用 %d 字节，配额 %d 字节。回收站或已被管理员下架、尚未彻底删除的图片仍占用配额。",
			used, quota),
	}
}

// imagesStoreUpload validates, thumbnails and persists one upload. Files are only
// kept when the database transaction commits; every other path removes them.
func (a *App) imagesStoreUpload(ctx context.Context, uploaderID int64, upload *imagesUpload) (*imagesUploadResponse, error) {
	head, err := imagesReadHead(upload.tempPath)
	if err != nil {
		return nil, fmt.Errorf("read staged upload: %w", err)
	}
	detected, ok := imagesDetectFormat(head)
	if !ok {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_FILE_TYPE", message: "仅支持 PNG、JPG、GIF、WEBP 图片"}
	}
	config, goFormatName, err := imagesDecodeConfig(upload.tempPath)
	if err != nil {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_IMAGE", message: "无法处理该图片，请尝试其它文件"}
	}
	goFormat, ok := imagesFormatForGoName(goFormatName)
	if !ok || goFormat.mime != detected.mime {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_FILE_TYPE", message: "仅支持 PNG、JPG、GIF、WEBP 图片"}
	}
	if err := imagesCheckDimensions(config.Width, config.Height, a.imagesMaxPixels()); err != nil {
		return nil, err
	}

	// 动画 WebP 的首帧由 imagesThumbnail 按 RFC 9649 单独合成；原图字节照常保留。
	animatedWebP := detected.mime == "image/webp" && imagesWebPAnimated(head)

	// Cheap quota check before decoding: do not spend CPU on an upload that cannot
	// be stored. The authoritative re-check happens inside the write transaction.
	quota, err := imagesQuotaBytes(ctx, a.db, uploaderID, a.cfg.UserQuotaBytes)
	if err != nil {
		return nil, err
	}
	if quota > 0 {
		used, err := imagesUsedBytes(ctx, a.db, uploaderID)
		if err != nil {
			return nil, err
		}
		if used+upload.size > quota {
			return nil, imagesQuotaError(used, quota)
		}
	}

	thumb, err := imagesThumbnail(upload.tempPath, animatedWebP, a.imagesMaxPixels())
	if err != nil {
		return nil, &imagesError{status: http.StatusBadRequest, code: "INVALID_IMAGE", message: "无法处理该图片，请尝试其它文件"}
	}

	id := uuid.NewString()
	createdAt := time.Now().UTC().Truncate(time.Millisecond)
	originalRel, thumbRel := imagesStoragePaths(uploaderID, id, createdAt, detected.ext)
	originalAbs, err := a.imagesSafePath(originalRel)
	if err != nil {
		return nil, fmt.Errorf("resolve original path: %w", err)
	}
	thumbAbs, err := a.imagesSafePath(thumbRel)
	if err != nil {
		return nil, fmt.Errorf("resolve thumbnail path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(originalAbs), 0o750); err != nil {
		return nil, fmt.Errorf("create image directory: %w", err)
	}
	// The staged file already lives under DataDir, so this is an atomic rename and
	// the original bytes are copied byte-for-byte.
	if err := os.Rename(upload.tempPath, originalAbs); err != nil {
		return nil, fmt.Errorf("store original image: %w", err)
	}
	upload.tempPath = ""

	stored := []string{originalAbs}
	cleanupFiles := func() {
		for _, target := range stored {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				logImagesCleanup("stored file", target, err)
			}
		}
	}
	if err := os.WriteFile(thumbAbs, thumb, 0o640); err != nil {
		cleanupFiles()
		return nil, fmt.Errorf("store thumbnail: %w", err)
	}
	stored = append(stored, thumbAbs)

	if err := a.imagesInsertImage(ctx, uploaderID, id, upload, detected, config, originalRel, thumbRel, createdAt, quota); err != nil {
		cleanupFiles()
		return nil, err
	}

	original, page, thumbURL := a.imagesURLs(id)
	response := &imagesUploadResponse{
		OK:       true,
		ID:       id,
		Tags:     upload.tags,
		URL:      original,
		PageURL:  page,
		ThumbURL: thumbURL,
	}
	if len(upload.tags) > 0 {
		first := upload.tags[0]
		response.Tag = &first
	}
	return response, nil
}

func (a *App) imagesInsertImage(
	ctx context.Context,
	uploaderID int64,
	id string,
	upload *imagesUpload,
	detected imagesFormat,
	config image.Config,
	originalRel string,
	thumbRel string,
	createdAt time.Time,
	fallbackQuota int64,
) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin image transaction: %w", err)
	}
	defer tx.Rollback()

	// Re-check the quota inside the transaction so two concurrent uploads cannot
	// both pass the fast check.
	quota, err := imagesQuotaBytes(ctx, tx, uploaderID, fallbackQuota)
	if err != nil {
		return err
	}
	if quota > 0 {
		used, err := imagesUsedBytes(ctx, tx, uploaderID)
		if err != nil {
			return err
		}
		if used+upload.size > quota {
			return imagesQuotaError(used, quota)
		}
	}

	storedAt := createdAt.Format(imagesTimeLayout)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO Image (id, uploaderId, originalName, originalMimeType, size, sha256, width, height,
			originalPath, thumbPath, createdAt, updatedAt, deletedAt, deletedByAdmin, isVisible)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, 1)`,
		id, uploaderID, upload.name, detected.mime, upload.size, upload.sha256,
		config.Width, config.Height, originalRel, thumbRel, storedAt, storedAt); err != nil {
		return fmt.Errorf("insert image: %w", err)
	}
	for _, name := range upload.tags {
		if _, err := tx.ExecContext(ctx, "INSERT INTO ImageTag (imageId, name) VALUES (?, ?)", id, name); err != nil {
			return fmt.Errorf("insert image tag: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit image: %w", err)
	}
	return nil
}
