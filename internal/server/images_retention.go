package server

// 回收站保留期清理：回收站中的图片（含管理员下架的记录）超过 TRASH_RETENTION_DAYS
// 之后由后台任务逐批物理删除。
//
// 边界：
//   - 复用 images_trash.go 的持久化清单机制，因此启动恢复、崩溃窗口与 cleanupPending
//     语义都与用户发起的彻底删除一致；
//   - 后台 goroutine 由 core 提供的 backgroundCancel / backgroundWG 管理，Close 先取消再等待，
//     不会在数据库关闭后继续清理；
//   - 启动后先等一小段再清理，每次只处理有限批次，避免启动时长时间占用数据库连接。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	imagesRetentionDefaultDays        = 30
	imagesRetentionBatchSize          = 50
	imagesRetentionMaxBatchesPerSweep = 20
)

// 定时参数是包级变量：生产路径使用默认值，测试可临时缩短以覆盖"启动即清理"与"Close 停止"。
var (
	imagesRetentionInitialDelay  = 30 * time.Second
	imagesRetentionSweepInterval = 6 * time.Hour
	imagesRetentionBatchPause    = 100 * time.Millisecond
)

// imagesRetentionSweepResult 是单次清理的真实计数，供日志与测试断言使用。
type imagesRetentionSweepResult struct {
	Scanned        int
	Purged         int
	Failed         int
	CleanupPending int
}

// imagesRetentionDays 解析保留天数：非正数回退到默认 30 天，0 不会关闭清理。
func (a *App) imagesRetentionDays() int {
	if a.cfg.TrashRetentionDays <= 0 {
		return imagesRetentionDefaultDays
	}
	return a.cfg.TrashRetentionDays
}

// imagesTrashCutoff 返回"已到期"的时间边界：deletedAt 早于该值的回收站记录需要清理。
func (a *App) imagesTrashCutoff(now time.Time) string {
	days := a.imagesRetentionDays()
	return now.UTC().Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Millisecond).Format(imagesTimeLayout)
}

// startTrashRetention 由 core 在启动恢复（imagesRecoverInterruptedPurges）完成后调用。
// 它建立后台 context/cancel 并交给 core 的 Close 统一停止。
func (a *App) startTrashRetention() {
	// 统一后台生命周期：context 与 cancel 由 core 在 New 中唯一创建（backgroundCtx /
	// backgroundCancel），邮件投递与这里的清理共用同一个 context。本函数不得覆盖
	// backgroundCancel，也不自建 parallel 的取消链；Close 统一 cancel → wait → db.Close。
	ctx := a.backgroundCtx
	if ctx == nil {
		// 没有共享 context 时（理论上的契约违约）不启动后台清理，而不是自建一个
		// Close 停不下来的 goroutine。
		return
	}
	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		a.imagesRunTrashRetention(ctx)
	}()
}

func (a *App) imagesRunTrashRetention(ctx context.Context) {
	timer := time.NewTimer(imagesRetentionInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		result := a.imagesTrashRetentionSweep(ctx)
		if result.Purged > 0 || result.Failed > 0 || result.CleanupPending > 0 {
			log.Printf("images: 回收站保留期清理完成: scanned=%d purged=%d failed=%d cleanupPending=%d",
				result.Scanned, result.Purged, result.Failed, result.CleanupPending)
		}
		timer.Reset(imagesRetentionSweepInterval)
	}
}

// imagesExpiredTrashIDs 取一批到期记录，按 deletedAt 升序（最旧的先清理）。
func (a *App) imagesExpiredTrashIDs(ctx context.Context, cutoff string, limit int) ([]string, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id FROM "Image" WHERE deletedAt IS NOT NULL AND deletedAt < ? ORDER BY deletedAt ASC, id ASC LIMIT ?`,
		cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("read expired trash images: %w", err)
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan expired trash image: %w", err)
		}
		ids = append(ids, id)
	}
	scanErr := rows.Err()
	// 单连接驱动：逐张彻底删除会开启新事务，必须先关闭结果集。
	rows.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("read expired trash images: %w", scanErr)
	}
	return ids, nil
}

// imagesTrashRetentionSweep 执行一轮有界清理：最多 imagesRetentionMaxBatchesPerSweep 批，
// 每批之间让出数据库连接。ctx 取消（Close）时立即返回，不再开始下一张。
func (a *App) imagesTrashRetentionSweep(ctx context.Context) imagesRetentionSweepResult {
	var result imagesRetentionSweepResult
	if ctx.Err() != nil {
		return result
	}
	cutoff := a.imagesTrashCutoff(time.Now())
	for batch := 0; batch < imagesRetentionMaxBatchesPerSweep; batch++ {
		if ctx.Err() != nil {
			return result
		}
		ids, err := a.imagesExpiredTrashIDs(ctx, cutoff, imagesRetentionBatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			log.Printf("images: 读取到期回收站图片失败: %v", err)
			return result
		}
		if len(ids) == 0 {
			return result
		}
		result.Scanned += len(ids)
		removed := 0
		for _, id := range ids {
			if ctx.Err() != nil {
				return result
			}
			cleanupPending, err := a.imagesPurgeTrashedByRetention(ctx, id)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return result
				}
				var requestErr *imagesError
				if errors.As(err, &requestErr) {
					// 记录已被用户恢复或手动彻底删除：不是失败，下一轮自然不会再选中。
					continue
				}
				log.Printf("images: 清理到期回收站图片 %s 失败: %v", id, err)
				result.Failed++
			default:
				result.Purged++
				removed++
				if cleanupPending {
					result.CleanupPending++
				}
			}
		}
		if removed == 0 {
			// 本批没有任何进展（例如全是并发恢复的记录），结束本轮，避免空转。
			return result
		}
		if !imagesRetentionSleep(ctx, imagesRetentionBatchPause) {
			return result
		}
	}
	return result
}

// imagesRetentionSleep 在等待期间响应取消；返回 false 表示已被取消。
func imagesRetentionSleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
