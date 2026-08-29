// Package openai 定义网关支持的 OpenAI 兼容 HTTP DTO。
package openai

// ChatCompletionRequest 表示聊天补全请求。
type ChatCompletionRequest struct {
	Model               string    `json:"model"`
	Messages            []Message `json:"messages"`
	Temperature         *float32  `json:"temperature,omitempty"`
	MaxCompletionTokens *int      `json:"max_completion_tokens,omitempty"`
	Stream              bool      `json:"stream,omitempty"`
}

// Message 表示 OpenAI 兼容的纯文本消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionResponse 表示非流式聊天补全响应。
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice 表示一条生成结果。
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

// Usage 表示上游明确返回的 Token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChunk 表示流式聊天补全 Chunk。
type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// StreamChoice 表示一条流式生成结果。
type StreamChoice struct {
	Index        int          `json:"index"`
	Delta        MessageDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

// MessageDelta 表示 OpenAI 兼容消息增量。
type MessageDelta struct {
	Role             *string `json:"role,omitempty"`
	Content          *string `json:"content,omitempty"`
	ReasoningContent *string `json:"reasoning_content,omitempty"`
}

// ErrorResponse 表示 OpenAI 兼容错误响应。
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 表示稳定且不含上游敏感信息的错误详情。
type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}
