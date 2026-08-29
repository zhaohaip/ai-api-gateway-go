package service

import (
	"fmt"
	"strings"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

// ModelRoute 表示一个逻辑模型对应的 Provider 和上游模型。
type ModelRoute struct {
	ExposedModel  string
	UpstreamModel string
	ProviderName  string
	Provider      ChatProvider
}

// ModelInfo 表示可对外公开的逻辑模型信息。
type ModelInfo struct {
	ID string
}

// ModelRegistry 定义聊天服务所需的只读模型路由能力。
type ModelRegistry interface {
	Resolve(model string) (ModelRoute, error)
	List() []ModelInfo
}

// Registry 是启动阶段构建、运行阶段只读的模型注册表。
type Registry struct {
	routes map[string]ModelRoute
	models []ModelInfo
}

// NewModelRegistry 校验并按输入顺序创建只读模型注册表。
func NewModelRegistry(routes []ModelRoute) (*Registry, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("at least one model route is required")
	}
	registry := &Registry{
		routes: make(map[string]ModelRoute, len(routes)),
		models: make([]ModelInfo, 0, len(routes)),
	}
	for index, route := range routes {
		route.ExposedModel = strings.TrimSpace(route.ExposedModel)
		route.UpstreamModel = strings.TrimSpace(route.UpstreamModel)
		route.ProviderName = strings.TrimSpace(route.ProviderName)
		if route.ExposedModel == "" {
			return nil, fmt.Errorf("model route %d exposed model is required", index)
		}
		if route.UpstreamModel == "" {
			return nil, fmt.Errorf("model route %q upstream model is required", route.ExposedModel)
		}
		if route.ProviderName == "" {
			return nil, fmt.Errorf("model route %q provider name is required", route.ExposedModel)
		}
		if route.Provider == nil {
			return nil, fmt.Errorf("model route %q provider is required", route.ExposedModel)
		}
		if _, exists := registry.routes[route.ExposedModel]; exists {
			return nil, fmt.Errorf("model route %q is duplicated", route.ExposedModel)
		}
		registry.routes[route.ExposedModel] = route
		registry.models = append(registry.models, ModelInfo{ID: route.ExposedModel})
	}
	return registry, nil
}

// Resolve 根据逻辑模型名精确匹配路由。
func (r *Registry) Resolve(model string) (ModelRoute, error) {
	route, exists := r.routes[model]
	if !exists {
		return ModelRoute{}, domain.NewModelNotFoundError(model)
	}
	return route, nil
}

// List 按注册顺序返回逻辑模型副本。
func (r *Registry) List() []ModelInfo {
	models := make([]ModelInfo, len(r.models))
	copy(models, r.models)
	return models
}
