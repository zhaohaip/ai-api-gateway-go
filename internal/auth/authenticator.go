package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// APIKeyAuthenticator 定义可替换的客户端 API Key 认证能力。
type APIKeyAuthenticator interface {
	Authenticate(ctx context.Context, rawKey string) (Principal, error)
}

type keyRecord struct {
	principal Principal
	enabled   bool
}

// MemoryAuthenticator 使用启动时构建的只读 Hash 索引认证 API Key。
type MemoryAuthenticator struct {
	keys map[[sha256.Size]byte]keyRecord
}

// NewMemoryAuthenticator 创建不保存客户端明文 Key 的内存认证器。
func NewMemoryAuthenticator(apiKeys []APIKey) (*MemoryAuthenticator, error) {
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("at least one client API key is required")
	}
	authenticator := &MemoryAuthenticator{keys: make(map[[sha256.Size]byte]keyRecord, len(apiKeys))}
	keyIDs := make(map[string]struct{}, len(apiKeys))
	for _, apiKey := range apiKeys {
		if apiKey.ID == "" {
			return nil, fmt.Errorf("client API key ID is required")
		}
		if _, exists := keyIDs[apiKey.ID]; exists {
			return nil, fmt.Errorf("client API key ID %q is duplicated", apiKey.ID)
		}
		if _, exists := authenticator.keys[apiKey.KeyHash]; exists {
			return nil, fmt.Errorf("client API key value is duplicated")
		}
		keyIDs[apiKey.ID] = struct{}{}
		authenticator.keys[apiKey.KeyHash] = keyRecord{
			principal: Principal{
				KeyID:         apiKey.ID,
				AllowedModels: append([]string(nil), apiKey.AllowedModels...),
			},
			enabled: apiKey.Enabled,
		}
	}
	return authenticator, nil
}

// Authenticate 对原始 Key 进行 Hash 后查找调用方。
func (a *MemoryAuthenticator) Authenticate(_ context.Context, rawKey string) (Principal, error) {
	record, exists := a.keys[sha256.Sum256([]byte(rawKey))]
	if !exists || !record.enabled {
		return Principal{}, NewAuthenticationError()
	}
	return clonePrincipal(record.principal), nil
}
