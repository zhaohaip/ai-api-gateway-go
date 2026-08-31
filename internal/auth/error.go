package auth

// ErrorKind 表示稳定的鉴权错误分类。
type ErrorKind string

const (
	// ErrorKindAuthentication 表示客户端身份认证失败。
	ErrorKindAuthentication ErrorKind = "authentication"
	// ErrorKindPermission 表示调用方无权访问目标模型。
	ErrorKindPermission ErrorKind = "permission"
)

// Error 表示可稳定映射为 HTTP 响应的鉴权错误。
type Error struct {
	Kind ErrorKind
}

// Error 返回不区分 Key 状态的安全错误说明。
func (e *Error) Error() string {
	if e.Kind == ErrorKindPermission {
		return "model access denied"
	}
	return "API key authentication failed"
}

// NewAuthenticationError 创建统一的 API Key 认证错误。
func NewAuthenticationError() *Error {
	return &Error{Kind: ErrorKindAuthentication}
}

// NewPermissionError 创建模型访问拒绝错误。
func NewPermissionError() *Error {
	return &Error{Kind: ErrorKindPermission}
}
