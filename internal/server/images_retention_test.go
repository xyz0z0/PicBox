package server

// 回收站保留期清理的回归：到期判定、管理员下架记录、后台启动/Close、与请求 purge
// 的并发竞态，以及取消 ctx 后不再继续清理。

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// imgRetentionTestBackdate 把一张图片的 deletedAt 改成 daysAgo 天前。
func imgRetentionTestBackdate(t *testing.T, env *imgTestEnv, imageID string, daysAgo int, adminRemoved bool) {
	t.Helper()
	deletedAt := time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour).Truncate(time.Millisecond).Format(imagesTimeLayout)
	admin := 0
	if adminRemoved {
		admin = 1
	}
	if _, err := env.app.db.Exec(`UPDATE "Image" SET deletedAt = ?, deletedByAdmin = ? WHERE id = ?`, deletedAt, admin, imageID); err != nil {
		t.Fatalf("回填 deletedAt 失败: %v", err)
	}
}

func imgRetentionTestRowExists(t *testing.T, env *imgTestEnv, imageID string) bool {
	t.Helper()
	var count int
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image" WHERE id = ?`, imageID).Scan(&count); err != nil {
		t.Fatalf("查询图片失败: %v", err)
	}
	return count > 0
}

func TestImagesTrashRetentionSweepPurgesExpired(t *testing.T) {
	env := imgTestNewApp(t, Config{TrashRetentionDays: 30})
	cookie := env.registerAdmin(t, "retention@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	expiredID, _ := imgTrashTestUpload(t, env, auth, "expired.png", []string{"old"})
	adminExpiredID, _ := imgTrashTestUpload(t, env, auth, "admin-expired.png", nil)
	recentID, _ := imgTrashTestUpload(t, env, auth, "recent.png", nil)
	activeID, _ := imgTrashTestUpload(t, env, auth, "active.png", nil)
	expiredOriginal, expiredThumb := imgTrashTestPaths(t, env, expiredID)

	imgTestTrash(t, env, auth, expiredID)
	imgRetentionTestBackdate(t, env, expiredID, 40, false)
	// 管理员下架：deletedByAdmin=1，不在用户回收站里，但同样占用磁盘，到期必须清理。
	imgRetentionTestBackdate(t, env, adminExpiredID, 40, true)
	imgTestTrash(t, env, auth, recentID)
	imgRetentionTestBackdate(t, env, recentID, 1, false)

	if env.app.cfg.TrashRetentionDays != 30 {
		t.Fatalf("默认保留天数 = %d，期望 30", env.app.cfg.TrashRetentionDays)
	}

	result := env.app.imagesTrashRetentionSweep(context.Background())
	if result.Scanned != 2 || result.Purged != 2 || result.Failed != 0 || result.CleanupPending != 0 {
		t.Fatalf("清理计数异常: %+v", result)
	}
	if imgRetentionTestRowExists(t, env, expiredID) || imgRetentionTestRowExists(t, env, adminExpiredID) {
		t.Fatalf("到期记录未被删除")
	}
	if !imgRetentionTestRowExists(t, env, recentID) || !imgRetentionTestRowExists(t, env, activeID) {
		t.Fatalf("未到期或未删除的图片被误删")
	}
	if !imgTrashTestFileMissing(t, expiredOriginal) || !imgTrashTestFileMissing(t, expiredThumb) {
		t.Fatalf("到期图片的字节未被物理删除")
	}
	if tags := imgTrashTestTags(t, env, expiredID); len(tags) != 0 {
		t.Fatalf("到期图片的标签残留: %v", tags)
	}
	if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
		t.Fatalf("保留期清理留下暂存残留: %v", leftovers)
	}

	// 回收站列表与用量都必须反映真实删除。
	bin := env.do(t, http.MethodGet, "/api/images?mine=true&trashed=true", nil, "", auth)
	if total := imgTrashTestTotal(t, bin); total != 1 {
		t.Fatalf("清理后回收站 total = %d，期望 1", total)
	}
	used := int64(imgTestJSON(t, imgExtraTestGet(t, env, "/api/images/usage", auth))["usedBytes"].(float64))
	var wantUsed int64
	if err := env.app.db.QueryRow(`SELECT SUM(size) FROM "Image"`).Scan(&wantUsed); err != nil {
		t.Fatalf("统计用量失败: %v", err)
	}
	if used != wantUsed {
		t.Fatalf("清理后用量 = %d，期望 %d", used, wantUsed)
	}
}

func TestImagesTrashRetentionHonorsRetentionWindow(t *testing.T) {
	env := imgTestNewApp(t, Config{TrashRetentionDays: 2})
	cookie := env.registerAdmin(t, "retention-window@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	threeDays, _ := imgTrashTestUpload(t, env, auth, "three-days.png", nil)
	oneDay, _ := imgTrashTestUpload(t, env, auth, "one-day.png", nil)
	imgTestTrash(t, env, auth, threeDays)
	imgRetentionTestBackdate(t, env, threeDays, 3, false)
	imgTestTrash(t, env, auth, oneDay)
	imgRetentionTestBackdate(t, env, oneDay, 1, false)

	result := env.app.imagesTrashRetentionSweep(context.Background())
	if result.Purged != 1 {
		t.Fatalf("2 天保留期只应清理 3 天前的记录: %+v", result)
	}
	if imgRetentionTestRowExists(t, env, threeDays) {
		t.Fatalf("3 天前的记录未被清理")
	}
	if !imgRetentionTestRowExists(t, env, oneDay) {
		t.Fatalf("1 天前的记录被误删")
	}

	// 非正数保留天数回退到默认 30 天，绝不等于"立即清理"。
	env.app.cfg.TrashRetentionDays = 0
	if days := env.app.imagesRetentionDays(); days != imagesRetentionDefaultDays {
		t.Fatalf("保留天数回退值 = %d，期望 %d", days, imagesRetentionDefaultDays)
	}
}

func TestImagesTrashRetentionStopsWhenContextCanceled(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "retention-cancel@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	imageID, _ := imgTrashTestUpload(t, env, auth, "cancel.png", nil)
	imgTestTrash(t, env, auth, imageID)
	imgRetentionTestBackdate(t, env, imageID, 40, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := env.app.imagesTrashRetentionSweep(ctx)
	if result.Scanned != 0 || result.Purged != 0 {
		t.Fatalf("已取消的 context 仍执行了清理: %+v", result)
	}
	if !imgRetentionTestRowExists(t, env, imageID) {
		t.Fatalf("已取消的 context 删除了图片")
	}
}

// TestImagesTrashRetentionBackgroundRunsAndCloseStops 覆盖后台循环的两个不变量：
// 启动后确实会清理（有界、异步），以及 Close 在清理进行中仍能及时返回。
func TestImagesTrashRetentionBackgroundRunsAndCloseStops(t *testing.T) {
	previousDelay, previousInterval, previousPause := imagesRetentionInitialDelay, imagesRetentionSweepInterval, imagesRetentionBatchPause
	imagesRetentionInitialDelay = 5 * time.Millisecond
	imagesRetentionSweepInterval = 50 * time.Millisecond
	imagesRetentionBatchPause = time.Millisecond
	t.Cleanup(func() {
		imagesRetentionInitialDelay = previousDelay
		imagesRetentionSweepInterval = previousInterval
		imagesRetentionBatchPause = previousPause
	})

	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "retention-bg@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	// 让清理有足够工作量，Close 才能真正发生在清理进行中。
	ids := make([]string, 0, 8)
	for index := 0; index < 8; index++ {
		imageID, _ := imgTrashTestUpload(t, env, auth, "bg-"+uuid.NewString()+".png", nil)
		imgTestTrash(t, env, auth, imageID)
		imgRetentionTestBackdate(t, env, imageID, 40, false)
		ids = append(ids, imageID)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		remaining := 0
		for _, id := range ids {
			if imgRetentionTestRowExists(t, env, id) {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("后台清理未在 10s 内完成，仍有 %d 张到期图片", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 再放一批到期图片，然后立刻 Close：Close 必须先取消再等待，且很快返回。
	for index := 0; index < 4; index++ {
		imageID, _ := imgTrashTestUpload(t, env, auth, "close-"+uuid.NewString()+".png", nil)
		imgTestTrash(t, env, auth, imageID)
		imgRetentionTestBackdate(t, env, imageID, 40, false)
	}
	started := time.Now()
	if err := env.app.Close(); err != nil {
		t.Fatalf("Close() 失败: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Close() 用了 %s，后台清理没有被可靠停止", elapsed)
	}
	// Close 已经等待了 backgroundWG，重复调用必须仍然安全（imgTestNewApp 的 cleanup 会再调一次）。
	if err := env.app.Close(); err != nil {
		t.Fatalf("重复 Close() 失败: %v", err)
	}
}

// TestImagesPurgeRaceWithRetention 覆盖请求 purge 与保留期清理同时处理同一张图片：
// 有界共享锁 + 单连接事务必须保证只删除一次、字节只搬移一次、没有残留。
func TestImagesPurgeRaceWithRetention(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "retention-race@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	for iteration := 0; iteration < 6; iteration++ {
		imageID, _ := imgTrashTestUpload(t, env, auth, "race-"+uuid.NewString()+".png", nil)
		originalAbs, thumbAbs := imgTrashTestPaths(t, env, imageID)
		imgTestTrash(t, env, auth, imageID)
		imgRetentionTestBackdate(t, env, imageID, 40, false)

		var wait sync.WaitGroup
		wait.Add(2)
		purgeResult := make(chan string, 1)
		go func() {
			defer wait.Done()
			recorder := imgTrashTestPurge(t, env, auth, imageID)
			purgeResult <- recorder.Body.String()
		}()
		go func() {
			defer wait.Done()
			env.app.imagesTrashRetentionSweep(context.Background())
		}()
		wait.Wait()
		close(purgeResult)

		if imgRetentionTestRowExists(t, env, imageID) {
			t.Fatalf("并发 purge 之后记录仍然存在（第 %d 轮）", iteration)
		}
		if !imgTrashTestFileMissing(t, originalAbs) || !imgTrashTestFileMissing(t, thumbAbs) {
			t.Fatalf("并发 purge 之后字节仍然存在（第 %d 轮）", iteration)
		}
		if leftovers := imgTrashTestLeftovers(t, env.app.cfg.DataDir); len(leftovers) != 0 {
			t.Fatalf("并发 purge 留下暂存残留（第 %d 轮）: %v", iteration, leftovers)
		}
	}
}
