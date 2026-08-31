package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMemoryAuthenticatorAuthenticatesEnabledKey(t *testing.T) {
	authenticator, err := NewMemoryAuthenticator([]APIKey{
		{
			ID:            "demo-client",
			KeyHash:       sha256.Sum256([]byte("sk-gw-demo")),
			Enabled:       true,
			AllowedModels: []string{"default-chat"},
		},
	})
	if err != nil {
		t.Fatalf("NewMemoryAuthenticator() error = %v", err)
	}

	principal, err := authenticator.Authenticate(context.Background(), "sk-gw-demo")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if principal.KeyID != "demo-client" || len(principal.AllowedModels) != 1 ||
		principal.AllowedModels[0] != "default-chat" {
		t.Fatalf("principal = %#v", principal)
	}
	principal.AllowedModels[0] = "mutated"
	again, err := authenticator.Authenticate(context.Background(), "sk-gw-demo")
	if err != nil || again.AllowedModels[0] != "default-chat" {
		t.Fatalf("stored principal was mutated: %#v, error = %v", again, err)
	}
}

func TestMemoryAuthenticatorHidesInvalidAndDisabledKeyState(t *testing.T) {
	authenticator, err := NewMemoryAuthenticator([]APIKey{
		{ID: "enabled", KeyHash: sha256.Sum256([]byte("enabled-key")), Enabled: true},
		{ID: "disabled", KeyHash: sha256.Sum256([]byte("disabled-key")), Enabled: false},
	})
	if err != nil {
		t.Fatalf("NewMemoryAuthenticator() error = %v", err)
	}
	for _, rawKey := range []string{"unknown-key", "disabled-key"} {
		_, authenticateErr := authenticator.Authenticate(context.Background(), rawKey)
		var authErr *Error
		if !errors.As(authenticateErr, &authErr) || authErr.Kind != ErrorKindAuthentication {
			t.Fatalf("Authenticate(%q) error = %#v", rawKey, authenticateErr)
		}
	}
	if strings.Contains(fmt.Sprintf("%#v", authenticator), "enabled-key") ||
		strings.Contains(fmt.Sprintf("%#v", authenticator), "disabled-key") {
		t.Fatal("authenticator retained a readable raw API key")
	}
}

func TestMemoryAuthenticatorRejectsDuplicateConfiguration(t *testing.T) {
	hash := sha256.Sum256([]byte("same-key"))
	tests := []struct {
		name string
		keys []APIKey
	}{
		{
			name: "duplicate ID",
			keys: []APIKey{
				{ID: "same", KeyHash: sha256.Sum256([]byte("first"))},
				{ID: "same", KeyHash: sha256.Sum256([]byte("second"))},
			},
		},
		{
			name: "duplicate value",
			keys: []APIKey{
				{ID: "first", KeyHash: hash},
				{ID: "second", KeyHash: hash},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewMemoryAuthenticator(test.keys); err == nil {
				t.Fatal("NewMemoryAuthenticator() accepted invalid configuration")
			}
		})
	}
}

func TestModelAuthorizerSupportsExplicitAndWildcardPermissions(t *testing.T) {
	authorizer := ModelAuthorizer{}
	if err := authorizer.Authorize(Principal{AllowedModels: []string{"default-chat"}}, "default-chat"); err != nil {
		t.Fatalf("explicit permission error = %v", err)
	}
	if err := authorizer.Authorize(Principal{AllowedModels: []string{"*"}}, "fast-chat"); err != nil {
		t.Fatalf("wildcard permission error = %v", err)
	}
	err := authorizer.Authorize(Principal{AllowedModels: []string{"default-chat"}}, "fast-chat")
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Kind != ErrorKindPermission {
		t.Fatalf("denied permission error = %#v", err)
	}
}

func TestPrincipalContextDoesNotExposeMutableState(t *testing.T) {
	ctx := ContextWithPrincipal(context.Background(), Principal{
		KeyID:         "client",
		AllowedModels: []string{"default-chat"},
	})
	principal, exists := PrincipalFromContext(ctx)
	if !exists {
		t.Fatal("PrincipalFromContext() did not find principal")
	}
	principal.AllowedModels[0] = "mutated"
	again, exists := PrincipalFromContext(ctx)
	if !exists || again.AllowedModels[0] != "default-chat" {
		t.Fatalf("context principal was mutated: %#v", again)
	}
}
