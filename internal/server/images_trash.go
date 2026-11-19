package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 回收站契约（第二阶段）：
//
//	GET    /api/images?mine=true&deleted=true  仅本人 Cookie，仅已软删记录，分页结构与图库一致
//	POST   /api/images/{id}/restore            本人 Cookie，撤销软删除，保留标签/可见性/原图字节
//	DELETE /api/images/{id}?permanent=true     本人 Cookie，仅回收站记录，删除数据库行并清理原图与缩略图
//
// 三个入口都不接受 API Key：回收站是账号私有视图，永久删除无法回滚。
// 他人图片、管理员下架（deletedByAdmin=1）与不存在的图片一律 404，避免泄露存在性。

const (
	// imagesPurgeQuarantinePrefix 命名暂存目录，失败后可人工辨认仍未清理的字节。
	imagesPurgeQuarantinePrefix = "purge-"
	// imagesPurgeManifestName 是暂存目录里的崩溃恢复清单，必须在移动任何文件之前落盘。
	imagesPurgeManifestName = "manifest.json"
	// imagesPurgeManifestTempSuffix 是清单发布（rename）之前的临时文件名后缀。
	// 只带这个文件、没有最终清单的残留目录对应"rename 之前"窗口，可以安全清理。
	imagesPurgeManifestTempSuffix = ".tmp"
)

// imagesPurgeManifest 只保存服务生成的数据：imageID 与相对数据目录的存储路径。
// 绝对路径不落盘；恢复时任何越界路径都拒绝执行，而不是照着清单删除或搬移。
type imagesPurgeManifest struct {
	ImageID string                    `json:"imageId"`
	Files   []imagesPurgeManifestFile `json:"files"`
}

type imagesPurgeManifestFile struct {
	Name     string `json:"name"`
	Relative string `json:"relative"`
}

func imagesNotFoundError() *imagesError {
	return &imagesError{status: http.StatusNotFound, code: "NOT_FOUND", message: "Not found"}
}

// imagesNotInTrashError 表示请求本身合法，但目标图片不在回收站。409 与 404 刻意区分：
// 调用者确实拥有这张图片，只是恢复/彻底删除只适用于回收站中的记录。
func imagesNotInTrashError(message string) *imagesError {
	return &imagesError{status: http.StatusConflict, code: "NOT_IN_TRASH", message: message}
}

func (a *App) imagesHandleRestore(w http.ResponseWriter, r *http.Request) {
	// Cookie only: an API key must not reach the owner's recycle bin.
	user, ok := a.authenticate(w, r, false)
	if !ok {
		return
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录")
		return
	}
	if err := a.imagesRestoreTrashed(r.Context(), user.ID, r.PathValue("id")); err != nil {
		imagesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// imagesRestoreTrashed clears deletedAt for one of the caller's own soft deletions.
// Tags, visibility and the stored bytes are untouched: restoring is a bookkeeping
// change, so a restored image is byte-identical to the one that was deleted.
func (a *App) imagesRestoreTrashed(ctx context.Context, userID int64, imageID string) error {
	if strings.TrimSpace(imageID) == "" {
		return imagesNotFoundError()
	}
	// The transaction holds the single database connection, so a concurrent purge of
	// the same image cannot interleave between the state check and the update.
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		uploaderID     int64
		deletedAt      *string
		deletedByAdmin int64
		originalRel    string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT uploaderId, deletedAt, deletedByAdmin, originalPath FROM Image WHERE id = ?`, imageID).
		Scan(&uploaderID, &deletedAt, &deletedByAdmin, &originalRel)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return imagesNotFoundError()
	case err != nil:
		return fmt.Errorf("read image for restore: %w", err)
	}
	if uploaderID != userID || deletedByAdmin != 0 {
		// Someone else's image, or one an administrator removed: not the caller's to
		// restore, and reporting 403 would confirm that the id exists.
		return imagesNotFoundError()
	}
	if deletedAt == nil {
		return imagesNotInTrashError("图片不在回收站中，无需恢复")
	}
	// Without the original bytes a restore would report success for an unreadable
	// image. Refuse it and leave the row in the recycle bin.
	originalAbs, err := a.imagesSafePath(originalRel)
	if err != nil {
		return fmt.Errorf("resolve original path for restore: %w", err)
	}
	if err := imagesRequireRegularFile(originalAbs); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE Image SET deletedAt = NULL, updatedAt = ? WHERE id = ? AND uploaderId = ? AND deletedAt IS NOT NULL AND deletedByAdmin = 0`,
		imagesNow(), imageID, userID)
	if err != nil {
		return fmt.Errorf("restore image: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count restored rows: %w", err)
	}
	if affected != 1 {
		return imagesNotInTrashError("图片不在回收站中，无需恢复")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restore: %w", err)
	}
	return nil
}

// imagesRequireRegularFile reports a 409 when the stored bytes are gone, and a wrapped
// error for any other stat failure so real I/O problems stay 500.
func imagesRequireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return &imagesError{status: http.StatusConflict, code: "IMAGE_BYTES_MISSING", message: "原图文件缺失，无法恢复该图片"}
	}
	return fmt.Errorf("stat stored image: %w", err)
}

// imagesPurgeLock 序列化「暂存 → 提交 → 清理字节」的物理文件窗口。数据库只有一条连接，
// 事务本身已经排队，但文件搬移发生在事务内部，请求 purge、批量 purge 与保留期清理都可能
// 同时走到这里；缓存这把有界锁让「同一时刻只有一个清理在执行」成为显式不变量，
// 也避免未来多连接配置下同一张图片被两个执行者搬移。锁在事务内部按固定顺序获取，
// 不会与自身持有的连接形成环。
var imagesPurgeLock sync.Mutex

func (a *App) imagesHandlePurge(w http.ResponseWriter, r *http.Request, user *User) {
	cleanupPending, err := a.imagesPurgeTrashed(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		imagesFail(w, err)
		return
	}
	// cleanupPending=true is honest about the split between the logical library and the
	// physical disk: the row, the tags and the canonical paths are gone, but the bytes
	// could not be removed from data/tmp and are cleaned on the next start.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleanupPending": cleanupPending})
}

// imagesPurgeTrashed removes one soft-deleted image for good.
//
// Files and rows cannot be committed together, so the smallest recoverable strategy is
// used:
//
//  1. start a transaction (it pins the single database connection, so a concurrent
//     restore or purge of the same image waits instead of interleaving),
//  2. verify ownership and that the row is soft-deleted,
//  3. write a manifest with the image id and the stored relative paths, then rename the
//     original and the thumbnail into data/tmp/purge-* (same filesystem, so no bytes
//     are copied). The manifest is durable before the first rename, which is what makes
//     a crash between the renames and the commit recoverable,
//  4. delete ImageTag rows and the Image row inside the transaction and commit,
//  5. only after the commit is durable, drop the quarantined bytes.
//
// Any failure before step 4 renames the files back and rolls back, so the caller keeps
// the image, its tags and its quota. A crash is repaired by
// imagesRecoverInterruptedPurges on the next start. A failure in step 5 only leaves
// bytes in data/tmp/purge-* (reported as cleanupPending) and is logged with the path.
func (a *App) imagesPurgeTrashed(ctx context.Context, userID int64, imageID string) (cleanupPending bool, err error) {
	return a.imagesPurgeTrashedScoped(ctx, userID, imageID, false)
}

// imagesPurgeTrashedByRetention 供回收站保留期清理使用：不限定 owner，并允许清理管理员下架
// 的回收站记录（它们同样占用磁盘，到期后必须被物理删除）。其余步骤与用户发起的彻底删除
// 完全一致，因此同样享有持久化清单与启动恢复。
func (a *App) imagesPurgeTrashedByRetention(ctx context.Context, imageID string) (cleanupPending bool, err error) {
	return a.imagesPurgeTrashedScoped(ctx, 0, imageID, true)
}

func (a *App) imagesPurgeTrashedScoped(ctx context.Context, userID int64, imageID string, retention bool) (cleanupPending bool, err error) {
	if strings.TrimSpace(imageID) == "" {
		return false, imagesNotFoundError()
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin purge transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		uploaderID     int64
		deletedAt      *string
		deletedByAdmin int64
		originalRel    string
		thumbRel       string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT uploaderId, deletedAt, deletedByAdmin, originalPath, thumbPath FROM Image WHERE id = ?`, imageID).
		Scan(&uploaderID, &deletedAt, &deletedByAdmin, &originalRel, &thumbRel)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, imagesNotFoundError()
	case err != nil:
		return false, fmt.Errorf("read image for purge: %w", err)
	}
	if !retention && (uploaderID != userID || deletedByAdmin != 0) {
		return false, imagesNotFoundError()
	}
	if deletedAt == nil {
		return false, imagesNotInTrashError("只能彻底删除回收站中的图片")
	}

	// 物理文件窗口只允许一个执行者：请求 purge、批量 purge 与保留期清理共用这把锁。
	imagesPurgeLock.Lock()
	defer imagesPurgeLock.Unlock()

	// Staging validates both stored paths and writes the manifest before touching the
	// files, so a row pointing outside the data directory fails with nothing renamed.
	quarantine, err := a.imagesStagePurge(imageID, []string{originalRel, thumbRel})
	if err != nil {
		return false, err
	}
	// putBack is called at most once, on every path that does not commit.
	putBack := func() { quarantine.restoreAll() }

	if _, err := tx.ExecContext(ctx, `DELETE FROM "ImageTag" WHERE imageId = ?`, imageID); err != nil {
		putBack()
		return false, fmt.Errorf("delete image tags: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM "Image" WHERE id = ? AND deletedAt IS NOT NULL`+imagesPurgeOwnerClause(retention),
		append([]any{imageID}, imagesPurgeOwnerArgs(userID, retention)...)...)
	if err != nil {
		putBack()
		return false, fmt.Errorf("delete image row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		putBack()
		return false, fmt.Errorf("count purged rows: %w", err)
	}
	if affected != 1 {
		putBack()
		return false, imagesNotInTrashError("只能彻底删除回收站中的图片")
	}
	if err := tx.Commit(); err != nil {
		putBack()
		return false, fmt.Errorf("commit purge: %w", err)
	}
	return quarantine.discard(), nil
}

// imagesPurgeOwnerClause / imagesPurgeOwnerArgs 把用户发起的彻底删除限定在「本人、非管理员下架」，
// 保留期清理不附加额外条件（它已经按到期时间筛选过）。
func imagesPurgeOwnerClause(retention bool) string {
	if retention {
		return ""
	}
	return " AND uploaderId = ? AND deletedByAdmin = 0"
}

func imagesPurgeOwnerArgs(userID int64, retention bool) []any {
	if retention {
		return nil
	}
	return []any{userID}
}

// imagesFileQuarantine 记录被移到暂存目录的文件，失败时按相反顺序搬回原处。
type imagesFileQuarantine struct {
	dir   string
	moved []imagesQuarantinedFile
}

type imagesQuarantinedFile struct {
	original string
	stashed  string
}

func imagesPurgeStashName(index int) string {
	return fmt.Sprintf("file-%d", index)
}

// imagesStagePurge resolves the stored paths, records them in a manifest, and only then
// moves the bytes aside. Everything it needs for recovery is on disk before the first
// rename, so the crash windows are limited to states the manifest can describe.
func (a *App) imagesStagePurge(imageID string, relativePaths []string) (*imagesFileQuarantine, error) {
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return nil, fmt.Errorf("data directory is not configured")
	}
	if strings.TrimSpace(imageID) == "" {
		return nil, fmt.Errorf("purge staging requires an image id")
	}
	// Validate every stored path before creating anything: a row that points outside
	// the data directory must fail without a manifest and without a single rename.
	absolute := make([]string, 0, len(relativePaths))
	for _, relative := range relativePaths {
		target, err := a.imagesSafePath(relative)
		if err != nil {
			return nil, fmt.Errorf("resolve stored path for purge: %w", err)
		}
		absolute = append(absolute, target)
	}
	dir, err := os.MkdirTemp(filepath.Join(a.cfg.DataDir, imagesTempDirName), imagesPurgeQuarantinePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create purge quarantine: %w", err)
	}
	manifest := imagesPurgeManifest{ImageID: imageID}
	for index, relative := range relativePaths {
		manifest.Files = append(manifest.Files, imagesPurgeManifestFile{
			Name:     imagesPurgeStashName(index),
			Relative: relative,
		})
	}
	if err := imagesWritePurgeManifest(dir, manifest); err != nil {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			logImagesCleanup("purge quarantine directory", dir, removeErr)
		}
		return nil, err
	}
	quarantine := &imagesFileQuarantine{dir: dir}
	for index, source := range absolute {
		target := filepath.Join(dir, imagesPurgeStashName(index))
		if err := os.Rename(source, target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Already gone: the manifest still lists it, and recovery treats a missing
				// stash entry as "already back in place".
				continue
			}
			quarantine.restoreAll()
			return nil, fmt.Errorf("quarantine %s: %w", source, err)
		}
		quarantine.moved = append(quarantine.moved, imagesQuarantinedFile{original: source, stashed: target})
	}
	return quarantine, nil
}

func imagesWritePurgeManifest(dir string, manifest imagesPurgeManifest) error {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode purge manifest: %w", err)
	}
	path := filepath.Join(dir, imagesPurgeManifestName)
	tmpPath := path + imagesPurgeManifestTempSuffix
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create purge manifest: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return fmt.Errorf("write purge manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync purge manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close purge manifest: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish purge manifest: %w", err)
	}
	// Sync the directory so the manifest entry itself survives a power loss, not just a
	// process crash.
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

// restoreAll moves every quarantined file back. A failed rename is logged with both
// paths: the caller reports the original error, and the bytes are still recoverable by
// hand from the returned location instead of being silently dropped.
func (q *imagesFileQuarantine) restoreAll() {
	if q == nil {
		return
	}
	complete := true
	for index := len(q.moved) - 1; index >= 0; index-- {
		file := q.moved[index]
		if err := os.Rename(file.stashed, file.original); err != nil {
			logImagesCleanup("quarantined file", file.stashed, err)
			complete = false
		}
	}
	q.moved = nil
	if !complete {
		// Keep the directory: it still holds bytes that could not be moved back.
		return
	}
	// RemoveAll: the directory still contains the manifest (and possibly its temp file).
	if err := os.RemoveAll(q.dir); err != nil {
		logImagesCleanup("purge quarantine directory", q.dir, err)
	}
	q.dir = ""
}

// discard drops the quarantined bytes after the row is gone and reports whether the
// cleanup is still pending. A pending cleanup is logged with the full path: the row,
// the tags and the canonical paths are already consistent, so this is bounded residue
// that the next start removes.
func (q *imagesFileQuarantine) discard() (cleanupPending bool) {
	if q == nil || q.dir == "" {
		return false
	}
	if err := os.RemoveAll(q.dir); err != nil {
		logImagesCleanup("purged image bytes", q.dir, err)
		return true
	}
	q.moved = nil
	q.dir = ""
	return false
}

// imagesRecoverInterruptedPurges repairs purge quarantines left by a crash between the
// file renames and the database commit, or between the commit and the byte cleanup. It
// runs before the router is registered, so no request can observe a half-purged image:
//
//   - the row still exists → the bytes are moved back to the paths the row points at,
//   - the row is gone → the quarantined bytes are removed.
//
// An unreadable manifest, a path that escapes the data directory, or a failed move is
// returned as an error that names the quarantine directory, and nothing is deleted: the
// operator gets evidence instead of a silently dropped image file.
func (a *App) imagesRecoverInterruptedPurges(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return fmt.Errorf("data directory is not configured")
	}
	tempDir := filepath.Join(a.cfg.DataDir, imagesTempDirName)
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取暂存目录 %s 失败: %w", tempDir, err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), imagesPurgeQuarantinePrefix) {
			continue
		}
		dir := filepath.Join(tempDir, entry.Name())
		if !entry.IsDir() {
			return fmt.Errorf("发现无法识别的回收站清理残留 %s，请人工确认后移除", dir)
		}
		if err := a.imagesRecoverPurgeDir(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) imagesRecoverPurgeDir(ctx context.Context, dir string) error {
	manifestPath := filepath.Join(dir, imagesPurgeManifestName)
	info, statErr := os.Lstat(manifestPath)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		// 没有最终清单说明进程崩在 imagesWritePurgeManifest 的 rename 之前（MkdirTemp 之后）：
		// 清单在任何 rename 之前落盘，因此这个窗口里不可能有字节被搬走，可以直接清理。
		// 但只有"空目录 / 仅常规 manifest.json.tmp"能证明这一点；其它内容一律保留现场。
		if err := imagesPurgeDirUnmoved(dir); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("清理未搬移字节的回收站清理残留失败（残留目录 %s）: %w", dir, err)
		}
		log.Printf("images: 已清理未开始搬移的回收站清理残留 %s（崩溃于 manifest 发布之前）", dir)
		return nil
	case statErr != nil:
		return fmt.Errorf("检查回收站清理清单失败（残留目录 %s）: %w", dir, statErr)
	case !info.Mode().IsRegular():
		// manifest.json 是符号链接/目录/设备文件时绝不跟随读取，也不清理现场。
		return fmt.Errorf("回收站清理清单不是常规文件（残留目录 %s），请人工确认后处理", dir)
	}

	manifest, stored, err := a.imagesReadPurgeManifest(dir)
	if err != nil {
		return err
	}
	var rows int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM Image WHERE id = ?`, manifest.ImageID).Scan(&rows); err != nil {
		return fmt.Errorf("恢复中断的图片删除失败（残留目录 %s）: %w", dir, err)
	}
	if rows == 0 {
		// The row was already purged; the quarantined bytes are residue.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("清理已删除图片的残留字节失败（残留目录 %s）: %w", dir, err)
		}
		return nil
	}
	// The row survived the crash, so its stored files belong back where it points.
	for index, file := range manifest.Files {
		stashed := filepath.Join(dir, file.Name)
		if _, err := os.Stat(stashed); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Renamed back before the crash, or never renamed.
				continue
			}
			return fmt.Errorf("恢复中断的图片删除失败（残留目录 %s）: %w", dir, err)
		}
		if err := os.MkdirAll(filepath.Dir(stored[index]), 0o750); err != nil {
			return fmt.Errorf("恢复中断的图片删除失败（残留目录 %s）: %w", dir, err)
		}
		if err := os.Rename(stashed, stored[index]); err != nil {
			return fmt.Errorf("恢复中断的图片删除失败（残留目录 %s）: %w", dir, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("清理恢复后的残留目录失败（%s）: %w", dir, err)
	}
	return nil
}

// imagesPurgeDirUnmoved 确认一个"没有最终清单"的 purge-* 目录确实还没搬移任何字节：
// 空目录，或只剩 imagesWritePurgeManifest 在 rename 之前写下的常规文件 manifest.json.tmp
// （内容可以是写了一半的，正是 partial tmp 写窗口）。任何其它条目——暂存字节 file-N、
// 未知文件、子目录、符号链接——都不能用"rename 之前崩溃"解释，必须保留现场交给人工。
func imagesPurgeDirUnmoved(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取回收站清理残留目录失败（残留目录 %s）: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == imagesPurgeManifestName {
			// 调用方用 Lstat 判定过缺失；若这里又出现，说明状态在两次检查之间变化。
			return fmt.Errorf("回收站清理清单状态不一致（残留目录 %s），请人工确认后处理", dir)
		}
		if name == imagesPurgeManifestName+imagesPurgeManifestTempSuffix && entry.Type().IsRegular() {
			continue
		}
		return fmt.Errorf("回收站清理残留目录 %s 含无法解释的条目 %q（缺少最终清单），请人工确认后处理", dir, name)
	}
	return nil
}

// imagesReadPurgeManifest parses and validates the manifest, including that every
// stored path still resolves inside the data directory. It returns the resolved
// absolute paths in the same order as manifest.Files.
func (a *App) imagesReadPurgeManifest(dir string) (imagesPurgeManifest, []string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, imagesPurgeManifestName))
	if err != nil {
		return imagesPurgeManifest{}, nil, fmt.Errorf("读取回收站清理清单失败（残留目录 %s）: %w", dir, err)
	}
	var manifest imagesPurgeManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return imagesPurgeManifest{}, nil, fmt.Errorf("解析回收站清理清单失败（残留目录 %s）: %w", dir, err)
	}
	if strings.TrimSpace(manifest.ImageID) == "" || len(manifest.Files) == 0 {
		return imagesPurgeManifest{}, nil, fmt.Errorf("回收站清理清单内容不完整（残留目录 %s）", dir)
	}
	stored := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		if !imagesPurgeStashNameValid(file.Name) {
			return imagesPurgeManifest{}, nil, fmt.Errorf("回收站清理清单包含非法文件名 %q（残留目录 %s）", file.Name, dir)
		}
		target, err := a.imagesSafePath(file.Relative)
		if err != nil {
			return imagesPurgeManifest{}, nil, fmt.Errorf("回收站清理清单包含非法存储路径 %q（残留目录 %s）: %w", file.Relative, dir, err)
		}
		stored = append(stored, target)
	}
	return manifest, stored, nil
}

func imagesPurgeStashNameValid(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return filepath.Base(name) == name
}
