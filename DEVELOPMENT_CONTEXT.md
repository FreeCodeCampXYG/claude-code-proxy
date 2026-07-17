# 开发交接与修改约束

> **修改本项目之前必须先通读本文档，并再读取涉及文件的当前实现与测试。**
>
> 本文档记录当前已确认的行为、用户决策和 Windows 发布约束；若旧文档、历史计划与本文档冲突，以当前代码、测试和本文档为准。不要根据 Claude 模型名称或旧设计推测行为。

## 1. 项目与发布环境

- 本项目将 Claude Messages API 转换到 OpenAI 兼容接口，主要链路是：
  `Claude Code -> /v1/messages -> converter -> 上游 NewAPI/OpenAI/OpenRouter/Ollama -> Claude 响应/SSE`。
- 用户使用 Windows 11，并只下载 GitHub Release 的 `claude-code-proxy-windows-amd64.exe`；**用户机器不需要安装 Go 或 SQLite**。
- `go.mod` 使用 Go 1.24；GitHub Actions 负责安装 Go、测试和构建。
- 标签发布工作流只有 [.github/workflows/go.yml](.github/workflows/go.yml)：Windows 测试和构建成功后，才进入 Ubuntu 的全平台发布。
- `modernc.org/sqlite` 是纯 Go SQLite 驱动，不能切换为需要 CGO 的驱动，否则会破坏 Windows 发布构建。
- Windows AMD64 EXE 图标在 CI 构建前由 `assets/windows/` 中的 ICO 和工作流固定版本的开源 `rsrc` 生成到包目录的 `rsrc_windows_amd64.syso`；构建必须针对 `./cmd/claude-code-proxy` 包而不是单独的 `main.go`，否则资源可能不会被链接。该 `.syso` 为 CI 临时构建产物，不提交到仓库；不需要 CGO、Windows SDK 或商业资源编译器。

## 2. 配置加载与 Provider 规则

配置文件按以下顺序查找并使用第一个存在的文件：

1. `./.env`
2. `%USERPROFILE%\\.claude\\proxy.env`（推荐）
3. `%USERPROFILE%\\.claude-code-proxy`

NewAPI（例如自建域名或本机 NewAPI）必须显式配置：

```env
OPENAI_BASE_URL=https://your-newapi.example/v1
OPENAI_PROVIDER=newapi
OPENAI_API_KEY=your-newapi-key
# 多账号手动选择：改用 OPENAI_API_KEYS=key-one,key-two，并设置 OPENAI_API_KEY_INDEX=1 或 2；切换后重启代理。
ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.6
```

- `OPENAI_PROVIDER` 可为 `openrouter`、`openai`、`ollama`、`newapi`、`generic`；显式值优先于 URL 自动识别。
- NewAPI 使用标准 OpenAI `/chat/completions` 路径，由 `Config.ChatCompletionsURL()` 统一构造；不能重新手写字符串拼接。
- NewAPI 流式请求应发送 `stream_options: {"include_usage": true}`。
- 不得向 NewAPI 混入 OpenRouter 专用 `reasoning` 对象，也不得混入 Ollama 的强制 `tool_choice: "required"` 行为。

## 3. GPT / NewAPI 推理强度：不可改变的策略

NewAPI 后端是 GPT-5.5/GPT-5.6。`reasoning_effort` 的规则必须保持透明转发：

1. 入站 Claude 请求有非空 `output_config.effort` 时，去除首尾空白、转换为小写后，原样转发到上游 `reasoning_effort`。
2. **不得**按 Claude Haiku / Sonnet / Opus 模型层级推断 effort。
3. **不得**按 `thinking.budget_tokens` 推断 effort。
4. 入站没有显式 effort 时，**不得发送** `reasoning_effort`，让 NewAPI/GPT 使用其默认值。
5. 不得将 `light` 改成 `low`，不得将 `xhigh` / `max` 改成 `high`，也不得限制未来值；如 `automatic`、`extra_high` 也应保留（仅做 trim + lowercase）。

相关实现与覆盖测试：

- [pkg/models/types.go](pkg/models/types.go)：接收 `output_config` 和 `thinking`。
- [internal/converter/converter.go](internal/converter/converter.go)：NewAPI 请求转换。
- [internal/converter/provider_test.go](internal/converter/provider_test.go)：effort 透传、空值省略、future value、流式 usage 测试。

## 4. 诊断记录、隐私与本机页面

诊断功能用于排查真实转发结构，但默认不启用。启用任一方式：

```powershell
# 方式一：本次启动启用
.\claude-code-proxy.exe -d

# 方式二：配置文件中长期启用
DIAGNOSTICS_ENABLED=true
```

启用后，启动输出必须包含：

```text
Diagnostics: http://127.0.0.1:8082/debug/logs
Diagnostics DB: C:\Users\<用户名>\AppData\Local\claude-code-proxy\diagnostics.db (retention 72h)
```

页面地址为 `http://127.0.0.1:<PORT>/debug/logs`。若无法打开，依次确认：

1. 当前进程是包含诊断功能的新 Release EXE，而不是旧二进制；
2. 代理已用 `-d` 或 `DIAGNOSTICS_ENABLED=true` 重新启动；配置只在启动时读取，先执行 `stop`；
3. URL 的端口与 `PORT` 一致；
4. 从代理所在 Windows 电脑的浏览器访问 `127.0.0.1`/`localhost`。该页面拒绝局域网、公网和伪造转发头访问。

默认数据库路径由 `os.UserCacheDir()` 生成，通常是：

```text
C:\Users\<用户名>\AppData\Local\claude-code-proxy\diagnostics.db
```

`DIAGNOSTICS_DB_PATH` 指向的父目录不存在时会自动创建。默认保留期为 `72h`，可用 `DIAGNOSTICS_RETENTION` 覆盖；启动时会清理过期记录。

### 默认不持久化的内容

默认诊断模式不得为了排查问题恢复保存或控制台打印以下原始内容：

- API key、`Authorization`、Cookie；
- 原始提示词、源码、工具参数值、工具结果；
- thinking / reasoning 正文；
- 原始 SSE 流分块。

默认可保存且应继续保持的内容包括：模型、provider、状态码、请求 ID、耗时、重试次数、字段是否存在、消息/工具数量、类型/长度、token usage、受限错误摘要及脱敏 JSON 结构。

### 显式本机内容抓取

只有同时配置 `DIAGNOSTICS_ENABLED=true` 和 `DIAGNOSTICS_CAPTURE_CONTENT=true` 时，才允许为本机排障短期保存内容快照；`-d` 只开启脱敏诊断，**不得**自动开启内容抓取。该选项保存的是入站 Claude 请求、实际发送的转换后上游请求、上游响应与回给 Claude Code 的响应四个边界。非流式仅保存有界的有效 JSON；流式仅保存归并后的语义内容，绝不保存可重放的原始 SSE 帧。

- 单份快照必须有严格大小上限；超长的有效 JSON 在完成脱敏和路径清理后只能保存首尾文本摘录、完整源字节长度、明确截断原因和省略字符数；malformed 内容仍只保存长度与解析错误；
- 不得保存请求头、API key、`Authorization`、Cookie；内容数据库/文件未加密，短期保留，且内容不进入列表、分析、普通详情或 NDJSON 导出；
- 内容接口仅供受保护的 loopback 诊断页面按需读取；
- 诊断是旁路能力：队列满、SQLite 繁忙或快照失败时，只允许丢弃诊断数据并输出脱敏告警，绝不阻塞、延迟或改变代理请求、重试、SSE 写入、取消与错误语义。

- 诊断请求 ID 由服务端重新生成，并回传给 Claude 客户端，同时以 `X-Request-ID` 转发上游；不信任客户端自带 ID。
- malformed JSON 只保存长度与解析错误，不保存原文。
- 测试中 `TestHandleMessagesMalformedBodyStoresMetadataOnly` 会故意制造 JSON 解析错误。控制台出现 `unexpected end of JSON input` 但测试显示 `PASS` 时，不是构建失败。
- SQLite 必须直接使用原生 Windows 文件路径：`sql.Open("sqlite", path)`。不要把 `C:\\...` 转成 `file://C:/...` URI；这会导致 Windows `modernc.org/sqlite` 初始化失败。
- 测试数据库使用 `t.TempDir()` 的系统临时目录，而非仓库目录，测试结束后由 Go 自动删除。

主要文件：

- [internal/diagnostics/](internal/diagnostics/)：SQLite schema、存储、脱敏、保留清理；
- [internal/server/diagnostics.go](internal/server/diagnostics.go)：页面、API、loopback 限制与请求追踪；
- [internal/server/handlers.go](internal/server/handlers.go)：请求生命周期、上游尝试和安全元数据。

## 5. Windows 守护进程约束

- PID 文件由 `os.UserCacheDir()` 决定，通常位于 `%LOCALAPPDATA%\\claude-code-proxy\\claude-code-proxy.pid`，不再使用 `/tmp`。
- 进程检查和停止按平台拆分到 `process_unix.go` 与 `process_windows.go`；不要把 Unix signal 代码移回 Windows 通用路径。
- `status` / `stop` 使用固定的 `http://localhost:8082/health`。修改 `PORT` 时，必须同时审视守护进程状态检查是否也要支持该端口。

## 6. 每次改动的最低流程

1. 通读本文档，再读取将修改的实现、调用方及现有测试；不要只根据历史计划修改。
2. 只做与当前需求直接相关的最小改动；不要顺带重构或格式化无关代码。
3. 涉及 NewAPI、参数转换或隐私数据时，先写/更新表驱动测试，再改实现。
4. 涉及 Windows 路径、SQLite 或守护进程时，必须保留 Windows CI 可执行性；临时文件应使用 `t.TempDir()`。
5. 提交前至少执行：

   ```bash
   go fmt ./...
   go test ./...
   go build -o claude-code-proxy.exe ./cmd/claude-code-proxy
   ```

6. 本机没有 Go 时，以 GitHub Actions 的 Windows `windows-smoke` 结果为准。工作流会运行 `go mod tidy` 并校验 `go.mod` / `go.sum` 无未提交变更；新增依赖时必须提交完整的 `go mod tidy` 结果，不能只补少量 checksum。

## 7. 当前已知状态

- NewAPI provider、GPT-5.6 transparent effort forwarding、SQLite 脱敏诊断、本机浏览器页面、Windows 原生 SQLite 路径与 Windows PID 处理均已实现并有相关测试。
- `-d` 必须启动新进程才会生效：先 `stop` 再用 `-d` 重启；通过 `/health` 的 `diagnostics_enabled` 确认页面是否已注册，而不会向远端暴露诊断数据。
- Claude SSE thinking delta 必须使用 `delta.thinking`，不可使用 `delta.text`；NewAPI/OpenAI 的 `reasoning_content` 必须在流式与非流式响应中都转换为 thinking。上游提供兼容 signature 时才透传，代理绝不伪造 signature。
- 发布版本由 `-ldflags` 注入，`version`、`/health` 与 `/` 必须显示同一实际 tag。
- 最近 Windows CI 已确认 `internal/config`、`internal/converter`、`internal/daemon`、`internal/diagnostics` 以及展示出的关键 `internal/server` 测试通过；工作流还应执行 EXE 的 `-d` 运行时诊断页面 smoke test。
- 后续改动前应再确认整个 `go test ./...` 和 Windows EXE build 的最终 job 结果，而不要仅根据日志中的单行 `Error:` 判断失败。
