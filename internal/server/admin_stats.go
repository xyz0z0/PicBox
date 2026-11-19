package server

// GET /api/admin/stats：管理端总览卡片与配额告急榜单。
//
// 语义与原版 app/api/admin/stats/route.ts 一致：
//   - imageCount 只统计仍在库中的图片（deletedAt IS NULL）；
//   - storageUsedBytes 统计全部图片记录（含回收站与管理員下架，物理删除前仍占磁盘）；
//   - todayUploadCount 按服务器本地 0 点；
//   - quotaAlertCount = 生效配额 > 0 且 used/quota >= 0.8 的账号数；
//   - quotaTop 取这些账号按 ratio 降序的前 5 个。
//
// 实现上全部用聚合查询，不读取图片字节，也不把图片行整表取回内存。

import (
	"database/sql"
	"net/http"
	"sort"
	"time"
)

const (
	adminQuotaAlertRatio = 0.8
	adminQuotaTopLimit   = 5
)

type adminStatsQuotaRow struct {
	ID         int64   `json:"id"`
	Label      string  `json:"label"`
	UsedBytes  int64   `json:"usedBytes"`
	QuotaBytes int64   `json:"quotaBytes"`
	Ratio      float64 `json:"ratio"`
	Exceeded   bool    `json:"exceeded"`
}

type adminStatsResponse struct {
	OK               bool                 `json:"ok"`
	UserCount        int64                `json:"userCount"`
	AdminCount       int64                `json:"adminCount"`
	DisabledCount    int64                `json:"disabledCount"`
	ImageCount       int64                `json:"imageCount"`
	StorageUsedBytes int64                `json:"storageUsedBytes"`
	TodayUploadCount int64                `json:"todayUploadCount"`
	QuotaAlertCount  int64                `json:"quotaAlertCount"`
	QuotaTop         []adminStatsQuotaRow `json:"quotaTop"`
}

func (a *App) adminHandleStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r); !ok {
		return
	}
	ctx := r.Context()

	response := adminStatsResponse{OK: true, QuotaTop: []adminStatsQuotaRow{}}

	counts := []struct {
		query string
		args  []any
		dest  *int64
	}{
		{`SELECT COUNT(*) FROM "User"`, nil, &response.UserCount},
		{`SELECT COUNT(*) FROM "User" WHERE role = ?`, []any{roleAdmin}, &response.AdminCount},
		{`SELECT COUNT(*) FROM "User" WHERE disabledAt IS NOT NULL`, nil, &response.DisabledCount},
		{`SELECT COUNT(*) FROM "Image" WHERE deletedAt IS NULL`, nil, &response.ImageCount},
		{`SELECT COALESCE(SUM(size), 0) FROM "Image"`, nil, &response.StorageUsedBytes},
		{`SELECT COUNT(*) FROM "Image" WHERE createdAt >= ?`, []any{adminStartOfToday()}, &response.TodayUploadCount},
	}
	for _, item := range counts {
		if err := a.db.QueryRowContext(ctx, item.query, item.args...).Scan(item.dest); err != nil {
			writeError(w, http.StatusInternalServerError, "STATS_FAILED", "读取站点统计失败")
			return
		}
	}

	// 单次聚合拿到"每个账号的用量"和"账号配额覆盖值"；账号数与图片数都是有界的聚合，
	// 不读取图片字节，也不在 Go 侧做 N+1 查询。
	rows, err := a.db.QueryContext(ctx,
		`SELECT u.id, COALESCE(NULLIF(u.email, ''), u.username), COALESCE(usage.used, 0), u.quotaBytes
		 FROM "User" u
		 LEFT JOIN (SELECT uploaderId, SUM(size) AS used FROM "Image" GROUP BY uploaderId) usage
		   ON usage.uploaderId = u.id`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STATS_FAILED", "读取配额统计失败")
		return
	}

	alertRows := make([]adminStatsQuotaRow, 0, 16)
	for rows.Next() {
		var (
			id       int64
			label    string
			used     int64
			override sql.NullInt64
		)
		if err := rows.Scan(&id, &label, &used, &override); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "STATS_FAILED", "读取配额统计失败")
			return
		}
		quotaBytes := a.cfg.UserQuotaBytes
		if override.Valid {
			quotaBytes = override.Int64
		}
		ratio := 0.0
		if quotaBytes > 0 {
			ratio = float64(used) / float64(quotaBytes)
		}
		if quotaBytes <= 0 || ratio < adminQuotaAlertRatio {
			continue
		}
		alertRows = append(alertRows, adminStatsQuotaRow{
			ID:         id,
			Label:      label,
			UsedBytes:  used,
			QuotaBytes: quotaBytes,
			Ratio:      ratio,
			Exceeded:   used >= quotaBytes,
		})
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		writeError(w, http.StatusInternalServerError, "STATS_FAILED", "读取配额统计失败")
		return
	}

	// ratio 降序，相同比率按 id 升序：榜单顺序稳定，翻页与刷新不会跳动。
	sort.Slice(alertRows, func(left, right int) bool {
		if alertRows[left].Ratio != alertRows[right].Ratio {
			return alertRows[left].Ratio > alertRows[right].Ratio
		}
		return alertRows[left].ID < alertRows[right].ID
	})
	response.QuotaAlertCount = int64(len(alertRows))
	if len(alertRows) > adminQuotaTopLimit {
		alertRows = alertRows[:adminQuotaTopLimit]
	}
	response.QuotaTop = alertRows

	writeJSON(w, http.StatusOK, response)
}

// adminStartOfToday 返回服务器本地 0 点对应的存储时间戳（Image.createdAt 的毫秒格式）。
// 按 UTC 与本地时区换算后再格式化，保证与库里的 TEXT 可直接比较。
func adminStartOfToday() string {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start.UTC().Format(imagesTimeLayout)
}
