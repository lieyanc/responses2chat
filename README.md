# responses2chat

一个无状态、无密钥库存的 Go 协议转换层：对 New API 暴露
`POST /v1/responses`，把请求转换为真正上游的
`POST /v1/chat/completions`，再把普通 JSON 或 SSE 流转换回 OpenAI
Responses API 形状。

```text
客户端（New API 发放的 Key）
  -> New API（鉴权、计费、渠道和真实上游 Key）
  -> responses2chat /v1/responses
  -> 模型上游 /v1/chat/completions
```

转换行为以 OpenAI 官方文档为准：Responses 使用 typed Items，Chat
Completions 使用 messages。无法由 Chat Completions 忠实表达的字段、Item、
内容部分和工具会逐项丢弃，不会原样泄漏给上游导致 unknown-field 错误。

## 启动

要求 Go 1.23 或更高版本。

```bash
export UPSTREAM_BASE_URL=https://model.example.com/v1
go run ./cmd/responses2chat
```

也可以指定完整地址：

```bash
export UPSTREAM_CHAT_COMPLETIONS_URL=https://model.example.com/v1/chat/completions
export LISTEN_ADDR=:8080
go run ./cmd/responses2chat
```

服务不会读取任何上游 Key 环境变量。New API 发给转换层的
`Authorization`、`x-api-key`、组织/项目头和其他端到端 HTTP 头会原样转发；
只移除 HTTP hop-by-hop 头并重新计算 `Content-Length`。

健康检查：`GET /healthz`。

## 请求转换

### 顶层字段

| Responses | Chat Completions | 行为 |
| --- | --- | --- |
| `model` | `model` | 透传 |
| `instructions` | 首条 `developer` message | 保留指令优先级 |
| `input` | `messages` | 按 Item/role/content 转换 |
| `max_output_tokens` | `max_completion_tokens` | 转换 |
| `reasoning.effort` | `reasoning_effort` | 转换；summary 丢弃 |
| `text.format` | `response_format` | `text`、`json_object`、`json_schema` |
| `text.verbosity` | `verbosity` | 转换 |
| `tools` | `tools` | 只保留 `function` |
| `tool_choice` | `tool_choice` | 字符串或指定函数形状转换 |
| `top_logprobs` | `top_logprobs` + `logprobs:true` | 转换 |
| `parallel_tool_calls`、`temperature`、`top_p`、`metadata`、`service_tier`、`prompt_cache_key`、`safety_identifier`、`user` | 同名字段 | 透传 |
| `store` | `store:false` | Responses 对象状态不可实现，显式禁用 |
| `stream:true` | `stream:true` + `stream_options.include_usage:true` | 强制请求终态 usage |
| `previous_response_id`、`conversation`、`prompt`、`background`、`context_management`、`include`、`max_tool_calls`、`truncation`、moderation/cache-only 配置 | 无忠实等价物 | 丢弃 |

### Context roles

完整处理 Responses message 支持的四种角色：

- `system` -> Chat `system`
- `developer` -> Chat `developer`
- `user` -> Chat `user`
- `assistant` -> Chat `assistant`
- `function_call_output` -> Chat `tool`，使用 `call_id` 关联

Responses 不把工具结果建模为 `role:"tool"` message，而是独立的
`function_call_output` Item，因此这里进行显式转换。

### Content parts

| Responses content | Chat content | 限制 |
| --- | --- | --- |
| `input_text` / `output_text` | `text` | 支持所有合法对应角色 |
| `refusal` | `refusal` | 仅 assistant |
| `input_image.image_url` | `image_url.url` | 仅 user；`file_id` 图片因 Chat 无等价物而丢弃 |
| `input_file.file_id` / `file_data` | `file` | 仅 user |
| `input_file.file_url` | 无 | 丢弃该 part |
| 工具输出中的文本 part | tool message 文本 part | 保留 |
| 工具输出中的图片/文件 part | 无 | 丢弃该 part |

如果一个 message 的全部 content parts 都无法表达，则丢弃整个 message；其他
同级 message 和 parts 仍会继续转换。

### Typed Items

- `message`：转换为对应 role 的 Chat message。
- `function_call`：转换为 assistant `tool_calls[]`；相邻并行函数调用会聚合到
  同一 assistant message。
- `function_call_output`：转换为 `tool` message。
- `reasoning`、`item_reference`、file/web/computer/code-interpreter/image-generation
  调用、shell/local-shell/apply-patch 调用及输出、MCP list/call/approval、custom
  tool 调用及输出，以及未来未知 Item：Chat Completions 无忠实表示，逐项丢弃。

## 响应转换

- Chat assistant 文本 -> Responses `message` + `output_text`。
- Chat refusal -> Responses `refusal` part。
- Chat `tool_calls[]` -> 独立 `function_call` output Items。
- URL annotations 和 token logprobs 在上游提供时保留。
- `prompt_tokens` / `completion_tokens` -> `input_tokens` / `output_tokens`，并保留
  cached/reasoning token details。
- `finish_reason:length` -> `status:incomplete`、
  `incomplete_details.reason:max_output_tokens`。
- `finish_reason:content_filter` -> incomplete/content_filter。
- 其他正常 finish reason -> completed。
- 非 2xx OpenAI JSON 错误保留 HTTP 状态和错误体；非 JSON 上游错误包装为标准
  `error` 对象。

## 流式转换

Chat Completions 的 `data:` chunks 会转换为带连续 `sequence_number` 的 typed
Responses SSE 事件，包括：

- `response.created` / `response.in_progress`
- `response.output_item.added` / `.done`
- `response.content_part.added` / `.done`
- `response.output_text.delta` / `.done`
- `response.refusal.delta` / `.done`
- `response.function_call_arguments.delta` / `.done`
- `response.completed` / `response.incomplete`
- `error` + `response.failed`

终态 Response 包含累计文本、函数参数和 usage。客户端断开连接时，请求 context
会取消上游请求。

## 明确不提供的状态能力

本服务是无状态透明层，不实现 Responses 的对象存储和检索。因此
`previous_response_id`、Conversations API、prompt template、后台任务以及 Hosted
Tools/MCP 的服务端 agent loop 不会被模拟。调用方需要把完整历史 Items 放入每次
`input`；可表达部分会转换为 Chat messages，不可表达部分按上面的规则丢弃。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试同时使用 OpenAI 官方 `github.com/openai/openai-go` 客户端验证普通 Responses
调用和 typed SSE 流可以被原生 SDK 解码。

## 官方参考

- [Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [Chat Completions create reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [Migrate to the Responses API](https://developers.openai.com/api/docs/guides/migrate-to-responses)
- [Function calling](https://developers.openai.com/api/docs/guides/function-calling)
