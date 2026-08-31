// Package auth 定义网关客户端身份认证和模型授权能力。
package auth

import "context"

// APIKey 表示启动配置解析后的安全 API Key 记录。
type APIKey struct {
	ID            string
	KeyHash       [32]byte
	Enabled       bool
	AllowedModels []string
}

// Principal 表示已通过认证的网关调用方。
type Principal struct {
	KeyID         string
	AllowedModels []string
}

type principalContextKey struct{}

// ContextWithPrincipal 将已认证调用方写入 Context。
func ContextWithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, clonePrincipal(principal))
}

// PrincipalFromContext 从 Context 读取已认证调用方。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, exists := ctx.Value(principalContextKey{}).(Principal)
	if !exists {
		return Principal{}, false
	}
	return clonePrincipal(principal), true
}

func clonePrincipal(principal Principal) Principal {
	models := make([]string, len(principal.AllowedModels))
	copy(models, principal.AllowedModels)
	principal.AllowedModels = models
	return principal
}
