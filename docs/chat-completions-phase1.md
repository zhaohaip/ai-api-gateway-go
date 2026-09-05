# Chat Completions Phase 1

当前阶段提供 `POST /v1/chat/completions` 的 OpenAI 兼容非流式与 SSE 流式代理。

## 配置

网关通过 `AI_GATEWAY_CONFIG_FILE` 指定 YAML 配置文件，结构参见 `configs/gateway.yaml.example`。`providers` 保存连接类型、Base URL、密钥环境变量名和传输层超时；`models` 保存逻辑模型名、Provider 引用和实际上游模型名。`AI_GATEWAY_ADDR` 可选覆盖 YAML 中的监听地址。

```bash
cp configs/gateway.yaml.example configs/gateway.yaml
export AI_GATEWAY_CONFIG_FILE=configs/gateway.yaml
export DEEPSEEK_API_KEY='your-key'
export QWEN_API_KEY='your-key'
export GATEWAY_DEMO_API_KEY='your-gateway-key'
export GATEWAY_INTERNAL_API_KEY='your-internal-gateway-key'
go run ./cmd/gateway
```

API Key 不写入 YAML。Provider API Key 继续通过 Provider 的 `api_key_env` 注入；客户端访问网关的 API Key 通过 `auth.api_keys[].key_env` 注入。客户端 Key 在加载后立即转换为 Hash，认证索引不保存可读取的明文值。配置会在启动时完整校验；配置无效、密钥缺失或 Provider 初始化失败时，进程直接启动失败。

每个客户端 Key 可以通过 `allowed_models` 指定可访问的逻辑模型，`"*"` 表示全部已注册模型。禁用 Key、未知 Key 和格式错误的 Bearer Header 都返回统一的 `401 invalid_api_key`；无权访问模型返回 `403 model_access_denied`。

`limits.global` 配置单实例全局请求频率，`limits.default_api_key` 配置每个 `Principal.KeyID` 的独立频率。两项都采用令牌桶，`requests_per_second` 表示令牌补充速率，`burst` 表示桶容量；同一项的两个字段都为 `0` 时禁用该项限制，其他情况下必须都大于 `0`。请求必须同时满足全局和 KeyID 额度，超限立即返回 `429 rate_limit_exceeded`，不会排队。可计算恢复时间时响应包含按秒向上取整的 `Retry-After`。

同一配置范围内的 `max_concurrency` 控制正在处理的请求数量，`0` 表示禁用该范围的并发限制，负数会导致启动失败。请求在频率限流通过后按“全局、KeyID”的顺序获取槽位，任一级达到上限都会立即返回 `429 concurrency_limit_exceeded`。非流式请求在 Handler 结束时释放；SSE 请求通过内部 Stream 包装器将槽位保持到 EOF、读取错误、取消或 Close，并确保重复关闭只释放一次。

`timeouts.non_stream` 控制非流式模型调用总时长；`timeouts.stream.first_chunk`、`idle` 和 `total` 分别控制 SSE 首个有效 Chunk、相邻有效输出的空闲时间和整个流的最大时长。任一业务超时值为 `0` 或省略时禁用对应限制，负数会导致启动失败。首个有效 Chunk 到达前不会提交 SSE 响应；首包超时可返回 `504 stream_first_chunk_timeout` JSON。响应已提交后的空闲或总时长超时只取消 Context、关闭流并释放并发槽，不追加 JSON 或 `[DONE]`。

Provider 使用 `connect_timeout`、`tls_handshake_timeout` 和 `response_header_timeout` 分别约束建连、TLS 握手和等待响应头，不设置会截断持续 SSE 输出的 `http.Client.Timeout`。旧版 Provider `timeout` 字段仍作为这三个传输超时的兼容回退值；新配置建议使用拆分字段。非流式和 SSE 业务超时由请求 Context 与 Stream 生命周期控制。

每个 Provider 的 Eino ChatModel 和底层 HTTP Client 在应用启动时创建一次。同一 Provider 下的多个逻辑模型复用该实例，并通过 Eino 模型选项传递各自的 `upstream_model`。客户端 Context 会沿 Handler、Service、Provider 传递到 Eino `Generate` 或 `Stream`。

## 兼容范围

请求仅接受 `model`、`messages`、`temperature`、`max_completion_tokens` 和 `stream`。消息只支持 `system`、`user`、`assistant` 三种角色和字符串文本内容。`stream=false` 使用非流式 JSON 响应，`stream=true` 使用 `text/event-stream` 逐块返回；未知字段返回 `unsupported_parameter`，不会静默忽略。逻辑模型必须与注册表精确匹配，未知模型不会降级到默认模型。

响应的 ID 由网关生成，`model` 始终是客户端逻辑模型名。同一次流式请求的 ID 和创建时间保持一致。只有 Eino 返回可靠 Token Usage 时才输出 `usage`；不会推测 Token 数或结束原因。

流式推理内容单独映射到 `delta.reasoning_content`，不会混入普通 `delta.content`。完全不含内容、推理内容、新角色、结束原因或 Usage 的空 Chunk 不会发送；Usage-only Chunk 仍会保留。

流式调用只有在收到结束原因并正常读到 EOF 后才发送 `data: [DONE]`。首个 Chunk 前的错误仍返回 OpenAI JSON 错误；已经输出部分 Chunk 后发生错误或取消时，连接直接结束，不追加 JSON 错误或伪造 `[DONE]`。

SSE 正常完成、网关超时、客户端取消和 Stream 错误通过同一个互斥终态决策竞争。正常完成胜出后停止全部业务计时器，再写出一次 `[DONE]`；异常终态胜出时不发送 `[DONE]`。终态与 Provider Context 的 Cause 一并固定，迟到的回调和取消不能覆盖结果。完成决策后的网络写失败只记录交付失败，不回退终态或重试结束标记。

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ${GATEWAY_DEMO_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"default-chat",
    "messages":[{"role":"user","content":"介绍一下AI API网关"}],
    "stream":true
  }'
```

模型列表只返回公开逻辑模型，不包含 Provider 名称、Base URL 或上游模型名：

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer ${GATEWAY_DEMO_API_KEY}"
```

`POST /v1/chat/completions` 和 `GET /v1/models` 都要求 `Authorization: Bearer <gateway-api-key>`，并计入请求频率和并发限制。模型列表只返回当前 Key 有权访问的逻辑模型。当前不支持 Key 管理接口、数据库存储、自动轮换、JWT、Token 配额或计费。

当前支持基于逻辑模型名的精确多模型路由、单实例内存请求限流和并发控制，以及非流式总超时和 SSE 首包、空闲、总时长超时；不支持多实例共享额度、请求排队、动态并发配额、权重路由、自动选择、自动重试和故障切换、Provider 健康检查、配置热更新、Token 配额、SSE 心跳、断线续传、Tool Calling 流式增量、Embedding、图片、音频、持久化、Chain、Graph 或 Agent。

客户端主动取消请求时使用 HTTP `499` 和 `request_canceled` 错误码；其余上游错误分别映射为 `429`、`504` 或 `502`，原始上游错误不会透传。
