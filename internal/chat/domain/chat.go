// Package domain 定义聊天网关的内部业务模型。
package domain

// MessageRole 表示内部消息角色。
type MessageRole string

const (
	// RoleSystem 表示系统消息。
	RoleSystem MessageRole = "system"
	// RoleUser 表示用户消息。
	RoleUser MessageRole = "user"
	// RoleAssistant 表示助手消息。
	RoleAssistant MessageRole = "assistant"
)

// Message 表示一条纯文本聊天消息。
type Message struct {
	Role    MessageRole
	Content string
}

// ChatRequest 表示网关内部的聊天生成请求。
type ChatRequest struct {
	Model               string
	Messages            []Message
	Temperature         *float32
	MaxCompletionTokens *int
}

// Usage 表示上游明确返回的 Token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatResponse 表示网关内部的聊天生成结果。
type ChatResponse struct {
	Model        string
	Message      Message
	FinishReason *string
	Usage        *Usage
}

// ChatDelta 表示一次流式响应中的消息增量。
type ChatDelta struct {
	Role             *MessageRole
	Content          *string
	ReasoningContent *string
}

// ChatChunk 表示一次流式聊天增量及其可靠元数据。
type ChatChunk struct {
	Delta        ChatDelta
	FinishReason *string
	Usage        *Usage
}

// Empty 返回当前 Chunk 是否不包含任何可输出内容或元数据。
func (c ChatChunk) Empty() bool {
	return c.Delta.Role == nil && c.Delta.Content == nil && c.Delta.ReasoningContent == nil &&
		c.FinishReason == nil && c.Usage == nil
}

// ChatStream 定义与具体模型 SDK 无关的流读取契约。
type ChatStream interface {
	Recv() (ChatChunk, error)
	Close() error
}
