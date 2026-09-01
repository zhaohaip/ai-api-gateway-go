// Package concurrencylimit 提供单实例内存请求并发控制。
package concurrencylimit

import (
	"fmt"
	"sync"
)

// Scope 表示触发并发限制的槽位范围。
type Scope string

const (
	// ScopeGlobal 表示网关实例的全局并发槽位。
	ScopeGlobal Scope = "global"
	// ScopeAPIKey 表示单个 Principal KeyID 的并发槽位。
	ScopeAPIKey Scope = "api_key"
)

// Error 表示某个范围的并发槽位已经耗尽。
type Error struct {
	Scope Scope
}

// Error 返回不包含客户端凭证的稳定错误说明。
func (e *Error) Error() string {
	return fmt.Sprintf("request concurrency limit exceeded for %s scope", e.Scope)
}

// Lease 表示一次已获取的并发槽位，Release 可安全重复调用。
type Lease interface {
	Release()
}

// Controller 定义立即获取并发槽位的最小接口。
type Controller interface {
	Acquire(keyID string) (Lease, error)
}

// MemoryController 使用进程内计数器组合全局和 KeyID 并发限制。
type MemoryController struct {
	mu          sync.Mutex
	globalMax   int
	globalInUse int
	apiKeyMax   int
	apiKeyInUse map[string]int
}

// NewMemoryController 创建并发安全的内存并发控制器；最大值为零时禁用对应限制。
func NewMemoryController(globalMax, apiKeyMax int) (*MemoryController, error) {
	if globalMax < 0 {
		return nil, fmt.Errorf("global max_concurrency must be non-negative")
	}
	if apiKeyMax < 0 {
		return nil, fmt.Errorf("default_api_key max_concurrency must be non-negative")
	}
	return &MemoryController{
		globalMax:   globalMax,
		apiKeyMax:   apiKeyMax,
		apiKeyInUse: make(map[string]int),
	}, nil
}

// Acquire 按全局、KeyID 的固定顺序获取槽位，任一级失败都会立即返回。
func (c *MemoryController) Acquire(keyID string) (Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	globalAcquired := false
	if c.globalMax > 0 {
		if c.globalInUse >= c.globalMax {
			return nil, &Error{Scope: ScopeGlobal}
		}
		c.globalInUse++
		globalAcquired = true
	}

	apiKeyAcquired := false
	if c.apiKeyMax > 0 {
		if c.apiKeyInUse[keyID] >= c.apiKeyMax {
			if globalAcquired {
				c.globalInUse--
			}
			return nil, &Error{Scope: ScopeAPIKey}
		}
		c.apiKeyInUse[keyID]++
		apiKeyAcquired = true
	}

	return &memoryLease{release: func() {
		c.release(keyID, globalAcquired, apiKeyAcquired)
	}}, nil
}

func (c *MemoryController) release(keyID string, globalAcquired, apiKeyAcquired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if apiKeyAcquired {
		if current := c.apiKeyInUse[keyID]; current > 1 {
			c.apiKeyInUse[keyID] = current - 1
		} else if current == 1 {
			delete(c.apiKeyInUse, keyID)
		}
	}
	if globalAcquired && c.globalInUse > 0 {
		c.globalInUse--
	}
}

type memoryLease struct {
	once    sync.Once
	release func()
}

func (l *memoryLease) Release() {
	l.once.Do(l.release)
}
