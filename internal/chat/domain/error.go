package domain

import "fmt"

// ErrorKind 是稳定的网关错误分类。
type ErrorKind string

const (
	// ErrorKindInvalidRequest 表示请求格式或参数错误。
	ErrorKindInvalidRequest ErrorKind = "invalid_request"
	// ErrorKindModelNotFound 表示逻辑模型不存在。
	ErrorKindModelNotFound ErrorKind = "model_not_found"
	// ErrorKindCanceled 表示客户端取消了请求。
	ErrorKindCanceled ErrorKind = "canceled"
	// ErrorKindRateLimited 表示上游触发限流。
	ErrorKindRateLimited ErrorKind = "rate_limited"
	// ErrorKindTimeout 表示上游调用超时。
	ErrorKindTimeout ErrorKind = "timeout"
	// ErrorKindUpstream 表示上游网络或服务异常。
	ErrorKindUpstream ErrorKind = "upstream"
	// ErrorKindInternal 表示网关内部异常。
	ErrorKindInternal ErrorKind = "internal"
)

// Error 表示可稳定映射为 HTTP 响应的领域错误。
type Error struct {
	Kind    ErrorKind
	Message string
	Param   string
	Code    string
	cause   error
}

// Error 返回不含敏感上游信息的错误说明。
func (e *Error) Error() string {
	return e.Message
}

// Unwrap 返回仅供内部错误判断使用的原始原因。
func (e *Error) Unwrap() error {
	return e.cause
}

// NewError 创建稳定分类的领域错误。
func NewError(kind ErrorKind, message, param, code string, cause error) *Error {
	return &Error{
		Kind:    kind,
		Message: message,
		Param:   param,
		Code:    code,
		cause:   cause,
	}
}

// NewInvalidRequestError 创建参数错误。
func NewInvalidRequestError(message, param, code string) *Error {
	return NewError(ErrorKindInvalidRequest, message, param, code, nil)
}

// NewInternalError 创建不泄露内部细节的网关异常。
func NewInternalError(cause error) *Error {
	return NewError(
		ErrorKindInternal,
		"the gateway encountered an internal error",
		"",
		"internal_error",
		fmt.Errorf("internal gateway error: %w", cause),
	)
}
