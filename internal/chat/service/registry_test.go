package service

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func TestModelRegistryRegistersMultipleProvidersAndModelsInStableOrder(t *testing.T) {
	sharedProvider := fakeProvider{}
	otherProvider := fakeProvider{}
	registry, err := NewModelRegistry([]ModelRoute{
		{ExposedModel: "default-chat", UpstreamModel: "deepseek-chat", ProviderName: "deepseek", Provider: sharedProvider},
		{ExposedModel: "reasoning-chat", UpstreamModel: "deepseek-reasoner", ProviderName: "deepseek", Provider: sharedProvider},
		{ExposedModel: "fast-chat", UpstreamModel: "qwen-plus", ProviderName: "qwen", Provider: otherProvider},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}

	models := registry.List()
	wantOrder := []string{"default-chat", "reasoning-chat", "fast-chat"}
	for index, want := range wantOrder {
		if models[index].ID != want {
			t.Fatalf("models[%d] = %q, want %q", index, models[index].ID, want)
		}
	}
	route, err := registry.Resolve("reasoning-chat")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if route.ProviderName != "deepseek" || route.UpstreamModel != "deepseek-reasoner" {
		t.Fatalf("route = %#v", route)
	}

	models[0].ID = "mutated"
	if registry.List()[0].ID != "default-chat" {
		t.Fatal("List() exposed mutable registry state")
	}
}

func TestModelRegistryRejectsDuplicateLogicalModel(t *testing.T) {
	provider := fakeProvider{}
	_, err := NewModelRegistry([]ModelRoute{
		{ExposedModel: "chat", UpstreamModel: "first", ProviderName: "provider", Provider: provider},
		{ExposedModel: "chat", UpstreamModel: "second", ProviderName: "provider", Provider: provider},
	})
	if err == nil {
		t.Fatal("NewModelRegistry() accepted duplicate logical model")
	}
}

func TestModelRegistryUnknownModelReturnsStableDomainError(t *testing.T) {
	registry, err := NewModelRegistry([]ModelRoute{
		{ExposedModel: "chat", UpstreamModel: "upstream", ProviderName: "provider", Provider: fakeProvider{}},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}

	_, err = registry.Resolve("unknown-model")
	if !errors.Is(err, domain.ErrModelNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrModelNotFound", err)
	}
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.ErrorKindModelNotFound ||
		domainErr.Param != "model" || domainErr.Code != "model_not_found" {
		t.Fatalf("Resolve() error = %#v", err)
	}
	if domainErr.Message != "The model 'unknown-model' does not exist." {
		t.Fatalf("Resolve() message = %q", domainErr.Message)
	}
}

func TestModelRegistrySupportsConcurrentReads(t *testing.T) {
	registry, err := NewModelRegistry([]ModelRoute{
		{ExposedModel: "first", UpstreamModel: "upstream-first", ProviderName: "provider", Provider: fakeProvider{}},
		{ExposedModel: "second", UpstreamModel: "upstream-second", ProviderName: "provider", Provider: fakeProvider{}},
	})
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}

	const readers = 32
	errorsChannel := make(chan error, readers)
	var waitGroup sync.WaitGroup
	for reader := 0; reader < readers; reader++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for iteration := 0; iteration < 100; iteration++ {
				route, resolveErr := registry.Resolve("second")
				if resolveErr != nil {
					errorsChannel <- resolveErr
					return
				}
				if route.UpstreamModel != "upstream-second" || len(registry.List()) != 2 {
					errorsChannel <- fmt.Errorf("unexpected registry data")
					return
				}
			}
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent registry read: %v", err)
	}
}
