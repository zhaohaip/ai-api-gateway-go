package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
)

func apiKeyAuthentication(authenticator gatewayauth.APIKeyAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawKey, err := bearerToken(c.Request.Header.Values("Authorization"))
		if err != nil {
			writeHTTPError(c, err)
			return
		}
		principal, err := authenticator.Authenticate(c.Request.Context(), rawKey)
		if err != nil {
			writeHTTPError(c, err)
			return
		}
		c.Request.Header.Del("Authorization")
		c.Request = c.Request.WithContext(
			gatewayauth.ContextWithPrincipal(c.Request.Context(), principal),
		)
		c.Next()
	}
}

func bearerToken(values []string) (string, error) {
	if len(values) != 1 {
		return "", gatewayauth.NewAuthenticationError()
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", gatewayauth.NewAuthenticationError()
	}
	return parts[1], nil
}

func writeHTTPError(c *gin.Context, err error) {
	status, response := toHTTPError(err)
	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", "Bearer")
	}
	c.AbortWithStatusJSON(status, response)
}
