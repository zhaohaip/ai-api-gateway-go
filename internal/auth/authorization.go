package auth

// ModelAuthorizer 执行与 HTTP 和模型路由无关的模型访问权限判断。
type ModelAuthorizer struct{}

// Authorize 校验调用方是否有权访问逻辑模型。
func (a ModelAuthorizer) Authorize(principal Principal, model string) error {
	if a.Allows(principal, model) {
		return nil
	}
	return NewPermissionError()
}

// Allows 返回调用方是否有权访问逻辑模型。
func (ModelAuthorizer) Allows(principal Principal, model string) bool {
	for _, allowedModel := range principal.AllowedModels {
		if allowedModel == "*" || allowedModel == model {
			return true
		}
	}
	return false
}
