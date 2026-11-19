package server

import (
	"sync"
	"time"
)

// rateLimiter 是有界、带过期回收的固定窗口计数器。
// 键由调用方（App.clientIP）给出：默认只使用直连对端地址，只有配置了
// TRUSTED_PROXIES 且直连对端在允许范围内时才使用 X-Forwarded-For 解析出的
// 真实客户端地址；任意客户端自带的转发头不能改变分桶。
// 容量满时优先回收过期条目，再淘汰最早重置的条目。
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*rateBucket
	limit    int
	window   time.Duration
	capacity int
	now      func() time.Time
}

type rateBucket struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration, capacity int) *rateLimiter {
	if capacity <= 0 {
		capacity = defaultRateLimitCapacity
	}
	return &rateLimiter{
		buckets:  make(map[string]*rateBucket),
		limit:    limit,
		window:   window,
		capacity: capacity,
		now:      time.Now,
	}
}

// allow 返回是否放行；被限流时同时返回距离窗口重置的等待时间。
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if bucket, ok := l.buckets[key]; ok {
		if now.Before(bucket.reset) {
			if bucket.count >= l.limit {
				return false, bucket.reset.Sub(now)
			}
			bucket.count++
			return true, 0
		}
		bucket.count = 1
		bucket.reset = now.Add(l.window)
		return true, 0
	}

	if len(l.buckets) >= l.capacity {
		l.evictLocked(now)
	}
	l.buckets[key] = &rateBucket{count: 1, reset: now.Add(l.window)}
	return true, 0
}

func (l *rateLimiter) evictLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if !now.Before(bucket.reset) {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < l.capacity {
		return
	}

	var (
		oldestKey   string
		oldestReset time.Time
	)
	for key, bucket := range l.buckets {
		if oldestKey == "" || bucket.reset.Before(oldestReset) {
			oldestKey = key
			oldestReset = bucket.reset
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
