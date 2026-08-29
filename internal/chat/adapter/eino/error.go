package eino

import (
	"context"
	"errors"
	"net"

	einopenai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/zhaohaip/ai-api-gateway-go/internal/chat/domain"
)

func classifyProviderError(err error) error {
	if errors.Is(err, context.Canceled) {
		return domain.NewError(
			domain.ErrorKindCanceled,
			"the request was canceled by the client",
			"",
			"request_canceled",
			err,
		)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return upstreamTimeoutError(err)
	}

	var apiErr *einopenai.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.HTTPStatusCode {
		case 408, 504:
			return upstreamTimeoutError(err)
		case 429:
			return domain.NewError(
				domain.ErrorKindRateLimited,
				"the upstream service rate limited the request",
				"",
				"upstream_rate_limited",
				err,
			)
		default:
			return upstreamServiceError(err)
		}
	}

	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return upstreamTimeoutError(err)
	}
	return upstreamServiceError(err)
}

func upstreamTimeoutError(cause error) *domain.Error {
	return domain.NewError(
		domain.ErrorKindTimeout,
		"the upstream service timed out",
		"",
		"upstream_timeout",
		cause,
	)
}

func upstreamServiceError(cause error) *domain.Error {
	return domain.NewError(
		domain.ErrorKindUpstream,
		"the upstream service could not complete the request",
		"",
		"upstream_error",
		cause,
	)
}
