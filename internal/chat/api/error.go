package api

import (
	"errors"
	"net/http"

	openaiapi "github.com/zhaohaip/ai-api-gateway-go/api/openai"
	gatewayauth "github.com/zhaohaip/ai-api-gateway-go/internal/auth"
	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

const statusClientClosedRequest = 499

func toHTTPError(err error) (int, openaiapi.ErrorResponse) {
	var authErr *gatewayauth.Error
	if errors.As(err, &authErr) {
		if authErr.Kind == gatewayauth.ErrorKindPermission {
			param := "model"
			return http.StatusForbidden, openaiapi.ErrorResponse{Error: openaiapi.ErrorDetail{
				Message: "You do not have access to this model.",
				Type:    "permission_error",
				Param:   &param,
				Code:    "model_access_denied",
			}}
		}
		return http.StatusUnauthorized, openaiapi.ErrorResponse{Error: openaiapi.ErrorDetail{
			Message: "Invalid API key.",
			Type:    "authentication_error",
			Param:   nil,
			Code:    "invalid_api_key",
		}}
	}

	domainErr := domain.NewInternalError(err)
	var classifiedErr *domain.Error
	if errors.As(err, &classifiedErr) {
		domainErr = classifiedErr
	}

	status, errorType := http.StatusInternalServerError, "server_error"
	switch domainErr.Kind {
	case domain.ErrorKindInvalidRequest:
		status, errorType = http.StatusBadRequest, "invalid_request_error"
	case domain.ErrorKindModelNotFound:
		status, errorType = http.StatusNotFound, "invalid_request_error"
	case domain.ErrorKindCanceled:
		status, errorType = statusClientClosedRequest, "request_canceled_error"
	case domain.ErrorKindRateLimited:
		status, errorType = http.StatusTooManyRequests, "rate_limit_error"
	case domain.ErrorKindTimeout:
		status, errorType = http.StatusGatewayTimeout, "upstream_timeout_error"
	case domain.ErrorKindUpstream:
		status, errorType = http.StatusBadGateway, "upstream_error"
	case domain.ErrorKindInternal:
		status, errorType = http.StatusInternalServerError, "server_error"
	}

	var param *string
	if domainErr.Param != "" {
		value := domainErr.Param
		param = &value
	}
	return status, openaiapi.ErrorResponse{
		Error: openaiapi.ErrorDetail{
			Message: domainErr.Message,
			Type:    errorType,
			Param:   param,
			Code:    domainErr.Code,
		},
	}
}
