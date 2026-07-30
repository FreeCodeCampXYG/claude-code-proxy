# Claude Code Proxy 中文说明

[![Latest Release](https://img.shields.io/github/v/release/nielspeter/claude-code-proxy?label=version)](https://github.com/nielspeter/claude-code-proxy/releases/latest)
[![Go Version](https://img.shields.io/github/go-mod/go-version/nielspeter/claude-code-proxy)](https://go.dev/)
[![Build Status](https://img.shields.io/github/actions/workflow/status/nielspeter/claude-code-proxy/ci.yml?branch=main)](https://github.com/nielspeter/claude-code-proxy/actions)
[![License](https://img.shields.io/github/license/nielspeter/claude-code-proxy)](LICENSE)
[![GitHub issues](https://img.shields.io/github/issues/nielspeter/claude-code-proxy)](https://github.com/nielspeter/claude-code-proxy/issues)

Claude Code Proxy 是一个轻量级 HTTP 代理，用于把 Claude Code 的请求转成 OpenAI 兼容上游请求，支持 OpenRouter、OpenAI Direct、自建 NewAPI 和 Ollama。

> **⚠️ 早期 / Beta 软件**
>
> 当前项目仍在快速迭代，核心能力已可用，但边缘场景和部分 provider 特性仍可能有问题，生产环境请谨慎使用。
>
> 欢迎反馈问题：<https://github.com/nielspeter/claude-code-proxy/issues>

## 功能

- ✅ **完整支持 Claude Code**
  - tool calling（read / write / edit / glob / grep / bash 等）
  - extended thinking / reasoning
  - 流式响应与实时 token 统计
  - 正确的 SSE 事件格式
- ✅ **多 Provider 支持**
  - **OpenRouter**：200+ 模型，通过单一 API 访问
  - **OpenAI Direct**：原生 GPT-5 reasoning 支持
  - **NewAPI**：自建 OpenAI 兼容网关，透明透传 Claude effort
  - **Ollama**：本地免费推理，适合离线使用
- ✅ **自适应模型能力检测**
  - 自动学习每个模型支持哪些参数
  - 不再依赖硬编码模型规则
  - 模型能力按 provider + model 缓存
- ✅ **Prompt 存档和诊断 UI**
  - 本机浏览器页面用于请求查看、搜索、导出
- ✅ **交互式 Playground**
  - 通用对话、OCR、PPT、图片工作流
- ✅ **本地控制台**
  - 聚合代理、监控、Prompt、设置、Playground 入口
- ✅ **模式路由**
  - 自动识别 Claude 模型并路由到目标上游模型
- ✅ **零依赖**
  - 单个约 10MB 二进制，无运行时依赖
- ✅ **守护进程模式**
  - 后台运行，服务多个 Claude Code 会话
- ✅ **启动快**
  - 冷启动非常快
- ✅ **配置灵活**
  - 支持 `~/.claude/proxy.env` 或 `.env`
- ✅ **直通模式**
  - 可直接转发到 Anthropic API 方便调试

## 快速开始

### 构建

```bash
go mod download
go build -o claude-code-proxy ./cmd/claude-code-proxy
make build
```

### 安装

#### Windows 发行版（无需 Go）

Windows EXE 使用开源 Lucide 图标生成的 Waypoints 风格应用图标。发布包是独立可运行文件，构建时才需要 Go。

从最新 Release 下载 `claude-code-proxy-windows-amd64.exe`，放到 PATH 后直接运行即可。

#### 方式 1：系统安装（推荐）

```bash
make install
```

会安装：

- `claude-code-proxy`
- `ccp` 包装脚本

#### 方式 2：手动安装

```bash
sudo cp claude-code-proxy /usr/local/bin/
sudo cp scripts/ccp /usr/local/bin/
sudo chmod +x /usr/local/bin/ccp
```

### 配置

当前支持四类上游：

#### 方案 1：OpenRouter（推荐）

```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=https://openrouter.ai/api/v1
OPENAI_API_KEY=sk-or-v1-your-openrouter-key

ANTHROPIC_DEFAULT_SONNET_MODEL=x-ai/grok-code-fast-1
ANTHROPIC_DEFAULT_HAIKU_MODEL=google/gemini-2.5-flash

OPENROUTER_APP_NAME=Claude-Code-Proxy
OPENROUTER_APP_URL=https://github.com/yourname/repo
EOF
```

#### 方案 2：OpenAI Direct

```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_API_KEY=sk-proj-your-openai-key

ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5-mini
ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5
EOF
```

#### 方案 3：自建 NewAPI

```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=http://127.0.0.1:3000/v1
OPENAI_PROVIDER=newapi
OPENAI_API_KEY=your-newapi-key

ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.6
EOF
```

需要显式设置 `OPENAI_PROVIDER=newapi`，避免 localhost 被自动识别成 Ollama。当前代理会把 Claude 的 `output_config.effort` 原样透传为 NewAPI `reasoning_effort`（仅 trim + lowercase）。如果没有显式 effort，则不发送 `reasoning_effort`，交给 NewAPI/GPT 默认值。

#### 方案 4：Ollama（本地）

```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=http://localhost:11434/v1
# 无需 API key

ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-r1:70b
ANTHROPIC_DEFAULT_HAIKU_MODEL=llama3.1:8b
EOF
```

## Provider 对比

| 特性 | OpenRouter | OpenAI Direct | NewAPI | Ollama |
|------|------------|---------------|--------|--------|
| 成本 | 按量付费 | 按量付费 | 自行部署决定 | 免费 |
| 搭建 | 简单 | 简单 | 自建 | 本地安装 |
| 模型 | 200+ | 仅 OpenAI | 取决于实例 | 开源模型 |
| 推理 | 支持 | 支持 | 支持 | 支持 |
| 工具调用 | 支持 | 支持 | 取决于模型 | 取决于模型 |
| 隐私 | 云端 | 云端 | 自托管 | 纯本地 |
| 速度 | 快 | 快 | 取决于部署 | 很快 |
| API Key | 需要 | 需要 | 取决于部署 | 不需要 |

## 运行

```bash
./claude-code-proxy
./claude-code-proxy status
./claude-code-proxy stop
./claude-code-proxy version
./claude-code-proxy help
```

常用参数：

```bash
-d, --debug   # 启用本机脱敏诊断（SQLite + /debug/logs）
-s, --simple  # 启用简洁日志
```

## 路由模式

| Claude 模型模式 | 默认 OpenAI 模型 |
|----------------|------------------|
| `*opus*` | `gpt-5` |
| `*sonnet*` | `gpt-5` |
| `*haiku*` | `gpt-5-mini` |

## 配置参考

### API 配置

- `OPENAI_BASE_URL`
- `OPENAI_API_KEY`
- `OPENAI_API_KEYS`
- `OPENAI_API_KEY_INDEX`
- `OPENAI_PROVIDER`
- `OPENROUTER_APP_NAME`
- `OPENROUTER_APP_URL`

### Playground 功能配置

- `OCR_MODEL`：可选的视觉 Chat Completions 模型，用于 Playground OCR；上传的 JPEG/PNG/WebP 仅在本次请求发送给上游，代理不持久化图片字节。
- `PPT_MODEL`：可选的 Playground PPT 大纲模型覆盖。
- `IMAGE_API_URL`、`IMAGE_API_KEY`、`IMAGE_MODEL`：三项必须同时配置才启用独立图片生成；图片 key 绝不回退复用 `OPENAI_API_KEY`。

### 诊断

- `DIAGNOSTICS_ENABLED`
- `DIAGNOSTICS_CAPTURE_CONTENT`
- `DIAGNOSTICS_CONTENT_RETENTION`
- `DIAGNOSTICS_DB_PATH`
- `DIAGNOSTICS_RETENTION`
- `DIAGNOSTICS_BUSY_TIMEOUT`

### Prompt 存档

- `PROMPT_ARCHIVE_ENABLED`
- `PROMPT_ARCHIVE_DB_PATH`
- `PROMPT_ARCHIVE_RETENTION`

### 服务器

- `HOST`
- `PORT`
- `PASSTHROUGH_MODE`

## 本机 Web UI

- `/`：控制台首页
- `/debug/logs`：诊断日志
- `/monitor`：流量监控
- `/prompts`：Prompt 存档
- `/playground`：交互式工作台

### Playground 结构

- **通用对话**：流式输出、停止生成、reasoning/text 分离、手动模型
- **图片工作流**：可选独立图片 API，返回有界本地预览（需同时配置 `IMAGE_API_URL`、`IMAGE_API_KEY`、`IMAGE_MODEL`，绝不回退复用聊天 key）
- **OCR**：上传 JPEG/PNG/WebP 到支持视觉的聊天模型（可选 `OCR_MODEL`）；代理不持久化图片字节
- **PPT**：校验 `ppt-outline/v1` 结构并提供预览、Markdown/HTML 导出（不生成 `.pptx`）
- **模型与 Key**：为多 Provider、多 Key、多接口预留
- **模板**：任务模板和 workflow 元数据

### Prompt 存档

Prompt 存档是独立功能：

- `PROMPT_ARCHIVE_ENABLED=true` 开启
- `PROMPT_ARCHIVE_DB_PATH` 指定数据库
- `PROMPT_ARCHIVE_RETENTION` 控制保留期

它会存储脱敏后的请求摘要与元数据，用于搜索、导出和排障。

## 构建发布

```bash
make build-all
```

## 项目结构

```text
proxy/
├── cmd/
├── internal/
│   ├── config/
│   ├── daemon/
│   ├── server/
│   └── converter/
├── pkg/
├── scripts/
└── Makefile
```

## 支持的 Claude Code 功能

- tool calling
- extended thinking
- streaming
- token tracking

## 开发

```bash
go run ./cmd/claude-code-proxy
go test ./...
go fmt ./...
```

## 手工测试

```bash
./claude-code-proxy -s &
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model opus -p "hi"
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model sonnet -p "hi"
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model haiku -p "hi"
```

## 工作方式

1. Claude Code 发送 Claude 格式请求到代理
2. 代理转成 OpenAI 格式
3. 代理转发到上游
4. 上游返回 OpenAI 响应
5. 代理转回 Claude 格式
6. Claude Code 收到响应

## 自适应模型检测

代理会自动学习每个模型支持哪些参数，并缓存结果。首次请求可能需要试探，之后会更快。

## 调试日志

启用 `-d` 后，可在本机打开诊断页面查看请求统计和记录。

## 许可证

MIT
