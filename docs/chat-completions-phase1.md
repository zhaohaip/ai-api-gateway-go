# Chat Completions Phase 1

当前阶段提供 `POST /v1/chat/completions` 的 OpenAI 兼容非流式与 SSE 流式代理。

## 配置

| 环境变量 | 必填 | 缺省值 | 说明 |
| --- | --- | --- | --- |
| `AI_GATEWAY_ADDR` | 否 | `:8080` | HTTP 监听地址 |
| `AI_GATEWAY_UPSTREAM_BASE_URL` | 否 | `https://api.openai.com/v1` | OpenAI 兼容上游 Base URL |
| `AI_GATEWAY_UPSTREAM_API_KEY` | 是 | - | 上游 API Key |
| `AI_GATEWAY_UPSTREAM_MODEL` | 是 | - | 实际调用的上游模型名 |
| `AI_GATEWAY_UPSTREAM_TIMEOUT` | 否 | `30s` | 上游请求超时，使用 Go Duration 格式 |
| `AI_GATEWAY_PUBLIC_MODEL` | 否 | `default-chat` | 客户端可见的唯一逻辑模型名 |

ChatModel 在应用启动时创建一次，由所有请求复用。客户端 Context 会沿 Handler、Service、Provider 传递到 Eino `Generate` 或 `Stream`。

## 兼容范围

请求仅接受 `model`、`messages`、`temperature`、`max_completion_tokens` 和 `stream`。消息只支持 `system`、`user`、`assistant` 三种角色和字符串文本内容。`stream=false` 使用非流式 JSON 响应，`stream=true` 使用 `text/event-stream` 逐块返回；未知字段返回 `unsupported_parameter`，不会静默忽略。

响应的 ID 由网关生成，`model` 始终是客户端逻辑模型名。同一次流式请求的 ID 和创建时间保持一致。只有 Eino 返回可靠 Token Usage 时才输出 `usage`；不会推测 Token 数或结束原因。

流式推理内容单独映射到 `delta.reasoning_content`，不会混入普通 `delta.content`。完全不含内容、推理内容、新角色、结束原因或 Usage 的空 Chunk 不会发送；Usage-only Chunk 仍会保留。

流式调用只有在收到结束原因并正常读到 EOF 后才发送 `data: [DONE]`。首个 Chunk 前的错误仍返回 OpenAI JSON 错误；已经输出部分 Chunk 后发生错误或取消时，连接直接结束，不追加 JSON 错误或伪造 `[DONE]`。

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"default-chat",
    "messages":[{"role":"user","content":"介绍一下AI API网关"}],
    "stream":true
  }'
```

当前不支持多模型路由、自动重试和故障切换、鉴权、网关限流/配额、SSE 心跳、Tool Calling 流式增量、Embedding、图片、音频、持久化、Chain、Graph 或 Agent。

客户端主动取消请求时使用 HTTP `499` 和 `request_canceled` 错误码；其余上游错误分别映射为 `429`、`504` 或 `502`，原始上游错误不会透传。
