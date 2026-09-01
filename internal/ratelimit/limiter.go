// Package ratelimit 提供单实例内存请求频率限制。
package ratelimit

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Scope 表示触发限流的额度范围。
type Scope string

const (
	// ScopeGlobal 表示网关实例的全局额度。
	ScopeGlobal Scope = "global"
	// ScopeAPIKey 表示单个 Principal KeyID 的额度。
	ScopeAPIKey Scope = "api_key"
)

// Limit 表示令牌桶的补充速率和容量；两个字段均为零时禁用该项限制。
type Limit struct {
	RequestsPerSecond float64
	Burst             int
}

// Error 表示请求超过了某个范围的频率限制。
type Error struct {
	Scope      Scope
	RetryAfter time.Duration
}

// Error 返回不包含客户端凭证的稳定错误说明。
func (e *Error) Error() string {
	return fmt.Sprintf("request rate limit exceeded for %s scope", e.Scope)
}

// Limiter 定义立即判断请求额度的最小接口。
type Limiter interface {
	Allow(keyID string) error
}

// MemoryLimiter 使用进程内令牌桶组合全局和 KeyID 请求额度。
type MemoryLimiter struct {
	mu          sync.Mutex
	global      *bucket
	apiKeyLimit Limit
	apiKeys     map[string]*bucket
	now         func() time.Time
}

// NewMemoryLimiter 创建并发安全的内存限流器。
func NewMemoryLimiter(global Limit, apiKey Limit) (*MemoryLimiter, error) {
	return newMemoryLimiter(global, apiKey, time.Now)
}

func newMemoryLimiter(global Limit, apiKey Limit, now func() time.Time) (*MemoryLimiter, error) {
	if err := validateLimit("global", global); err != nil {
		return nil, err
	}
	if err := validateLimit("default_api_key", apiKey); err != nil {
		return nil, err
	}
	limiter := &MemoryLimiter{
		apiKeyLimit: apiKey,
		apiKeys:     make(map[string]*bucket),
		now:         now,
	}
	if enabled(global) {
		limiter.global = newBucket(global, now())
	}
	return limiter, nil
}

// Allow 在全局和 KeyID 额度均可用时扣减令牌，否则立即返回限流错误。
func (l *MemoryLimiter) Allow(keyID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.global != nil {
		l.global.refill(now)
		if retryAfter := l.global.retryAfter(); retryAfter > 0 {
			return &Error{Scope: ScopeGlobal, RetryAfter: retryAfter}
		}
	}

	var apiKeyBucket *bucket
	if enabled(l.apiKeyLimit) {
		apiKeyBucket = l.apiKeys[keyID]
		if apiKeyBucket == nil {
			apiKeyBucket = newBucket(l.apiKeyLimit, now)
			l.apiKeys[keyID] = apiKeyBucket
		}
		apiKeyBucket.refill(now)
		if retryAfter := apiKeyBucket.retryAfter(); retryAfter > 0 {
			return &Error{Scope: ScopeAPIKey, RetryAfter: retryAfter}
		}
	}

	if l.global != nil {
		l.global.tokens--
	}
	if apiKeyBucket != nil {
		apiKeyBucket.tokens--
	}
	return nil
}

type bucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(limit Limit, now time.Time) *bucket {
	burst := float64(limit.Burst)
	return &bucket{
		rate:   limit.RequestsPerSecond,
		burst:  burst,
		tokens: burst,
		last:   now,
	}
}

func (b *bucket) refill(now time.Time) {
	if now.Before(b.last) {
		return
	}
	b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
}

func (b *bucket) retryAfter() time.Duration {
	if b.tokens >= 1 {
		return 0
	}
	seconds := (1 - b.tokens) / b.rate
	nanoseconds := math.Ceil(seconds * float64(time.Second))
	const maximumDuration = time.Duration(1<<63 - 1)
	if nanoseconds >= float64(maximumDuration) {
		return maximumDuration
	}
	return time.Duration(nanoseconds)
}

func validateLimit(name string, limit Limit) error {
	if math.IsNaN(limit.RequestsPerSecond) || math.IsInf(limit.RequestsPerSecond, 0) ||
		limit.RequestsPerSecond < 0 {
		return fmt.Errorf("%s requests_per_second must be a finite non-negative number", name)
	}
	if limit.Burst < 0 {
		return fmt.Errorf("%s burst must be non-negative", name)
	}
	if (limit.RequestsPerSecond == 0) != (limit.Burst == 0) {
		return fmt.Errorf("%s requests_per_second and burst must both be zero or greater than zero", name)
	}
	return nil
}

func enabled(limit Limit) bool {
	return limit.RequestsPerSecond > 0
}
