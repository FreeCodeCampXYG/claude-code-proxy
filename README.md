# Claude Code Proxy (Go)

[![Latest Release](https://img.shields.io/github/v/release/nielspeter/claude-code-proxy?label=version)](https://github.com/nielspeter/claude-code-proxy/releases/latest)
[![Go Version](https://img.shields.io/github/go-mod/go-version/nielspeter/claude-code-proxy)](https://go.dev/)
[![Build Status](https://img.shields.io/github/actions/workflow/status/nielspeter/claude-code-proxy/ci.yml?branch=main)](https://github.com/nielspeter/claude-code-proxy/actions)
[![License](https://img.shields.io/github/license/nielspeter/claude-code-proxy)](LICENSE)
[![GitHub issues](https://img.shields.io/github/issues/nielspeter/claude-code-proxy)](https://github.com/nielspeter/claude-code-proxy/issues)

A lightweight HTTP proxy that enables Claude Code to work with OpenAI-compatible API providers including OpenRouter (200+ models), OpenAI Direct (GPT-5 reasoning), self-hosted NewAPI, and Ollama (free local inference).

> **⚠️ Early Stage / Beta Software**
>
> This project is in active development and has limited production testing. While core functionality works, edge cases and some provider-specific features may have issues. Use with caution in production environments.
>
> **Feedback welcome!** Please report issues at https://github.com/nielspeter/claude-code-proxy/issues

## Features

- ✅ **Full Claude Code Compatibility** - Complete support for all Claude Code features
  - Tool calling (read, write, edit, glob, grep, bash, etc.)
  - Extended thinking blocks with proper hiding/showing
  - Streaming responses with real-time token tracking
  - Proper SSE event formatting
- ✅ **Multiple Provider Support** - OpenRouter, OpenAI Direct, self-hosted NewAPI, and Ollama
  - **OpenRouter**: 200+ models (GPT, Grok, Gemini, etc.) through a single API
  - **OpenAI Direct**: Native GPT-5 reasoning model support
  - **NewAPI**: Self-hosted OpenAI-compatible gateway with transparent Claude effort forwarding
  - **Ollama**: Free local inference with DeepSeek-R1, Llama3, Qwen, etc.
- ✅ **Adaptive Per-Model Detection** - Zero-config provider compatibility
  - Automatically learns which parameters each model supports
  - No hardcoded model patterns - works with future model/provider changes
  - Per-model capability caching for instant subsequent requests
- ✅ **Prompt archive and diagnostics UI** - Local-only browser tools for request inspection and export
- ✅ **Interactive Playground** - Multi-stage prompt, OCR, PPT, and image workflow workbench
- ✅ **Local dashboard** - Single-entry navigation for proxy, monitor, prompts, settings, and playground pages
- ✅ **Pattern-based routing** - Auto-detects Claude models and routes to appropriate backend models
- ✅ **Zero dependencies** - Single ~10MB binary, no runtime needed
- ✅ **Daemon mode** - Runs in background, serves multiple Claude Code sessions
- ✅ **Fast startup** - < 10ms cold start
- ✅ **Config flexibility** - Loads from `~/.claude/proxy.env` or `.env`
- ✅ **Passthrough mode** - Optional direct proxying to Anthropic API for debugging

## Quick Start

### Build

```bash
# Install dependencies
go mod download

# Build binary
go build -o claude-code-proxy ./cmd/claude-code-proxy

# Or use make
make build
```

### Install

**Windows release (no Go required)**

The Windows EXE includes a Waypoints-based application icon derived from [Lucide Icons](https://lucide.dev/) under the ISC License. It is an independent third-party proxy icon and is not a Claude Code or Anthropic mark.

Download `claude-code-proxy-windows-amd64.exe` from the [latest release](https://github.com/nielspeter/claude-code-proxy/releases/latest), place it on your `PATH`, and run it directly. The release EXE is self-contained; Go is required only when building from source.

**Option 1: System-wide installation (recommended)**

```bash
# Install binary and ccp wrapper to /usr/local/bin
make install

# This installs:
#   - claude-code-proxy (main binary)
#   - ccp (wrapper script for easy usage)
```

**Option 2: Manual installation**

```bash
# Copy binary to PATH
sudo cp claude-code-proxy /usr/local/bin/

# Copy wrapper script (optional but recommended)
sudo cp scripts/ccp /usr/local/bin/
sudo chmod +x /usr/local/bin/ccp
```

After installation, `claude-code-proxy` and `ccp` will be available system-wide.

### Configuration

The proxy supports four provider types. Choose the one that fits your needs:

**Option 1: OpenRouter (Recommended)**
```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=https://openrouter.ai/api/v1
OPENAI_API_KEY=sk-or-v1-your-openrouter-key

# Model routing
ANTHROPIC_DEFAULT_SONNET_MODEL=x-ai/grok-code-fast-1
ANTHROPIC_DEFAULT_HAIKU_MODEL=google/gemini-2.5-flash

# Optional: Better rate limits
OPENROUTER_APP_NAME=Claude-Code-Proxy
OPENROUTER_APP_URL=https://github.com/yourname/repo
EOF
```

**Option 2: OpenAI Direct**
```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_API_KEY=sk-proj-your-openai-key

# Model routing
ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5-mini
ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5  # Reasoning model
EOF
```

**Option 3: Self-hosted NewAPI**
```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=http://127.0.0.1:3000/v1
OPENAI_PROVIDER=newapi
OPENAI_API_KEY=your-newapi-key

# Map Claude tiers to model IDs exposed by your NewAPI instance
ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.6
EOF
```

Set `OPENAI_PROVIDER=newapi` explicitly: localhost URLs would otherwise be auto-detected as Ollama, while custom domains would be treated as generic providers. The proxy forwards Claude `output_config.effort` to NewAPI `reasoning_effort` exactly as provided after trim+lowercase. If Claude Code does not send `output_config.effort`, the proxy omits `reasoning_effort` and lets NewAPI/GPT use its configured default. `thinking.budget_tokens` is not converted into an effort level.

**Option 4: Ollama (Local)**
```bash
mkdir -p ~/.claude
cat > ~/.claude/proxy.env << 'EOF'
OPENAI_BASE_URL=http://localhost:11434/v1
# No API key needed!

# Model routing
ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-r1:70b
ANTHROPIC_DEFAULT_HAIKU_MODEL=llama3.1:8b
EOF
```

## Provider Comparison

| Feature | OpenRouter | OpenAI Direct | NewAPI | Ollama |
|---------|-----------|---------------|--------|--------|
| **Cost** | Pay-per-use | Pay-per-use | Deployment dependent | Free |
| **Setup** | Easy | Easy | Self-hosted | Requires local install |
| **Models** | 200+ | OpenAI only | Instance dependent | Open source only |
| **Reasoning** | Yes (via GPT/Grok/etc) | Yes (GPT-5) | Yes (transparent effort forwarding) | Yes (DeepSeek-R1) |
| **Tool Calling** | Yes | Yes | Model dependent | Model dependent |
| **Privacy** | Cloud | Cloud | Self-hosted | 100% local |
| **Speed** | Fast | Fast | Deployment dependent | Very fast (local) |
| **API Key** | Required | Required | Deployment dependent | Not needed |

### Run

**Commands:**

```bash
./claude-code-proxy              # Start daemon
./claude-code-proxy status       # Check if running
./claude-code-proxy stop         # Stop daemon
./claude-code-proxy version      # Show version
./claude-code-proxy help         # Show help
```

**Flags:**

```bash
-d, --debug     # Enable local redacted diagnostics (SQLite + /debug/logs)
-s, --simple    # Enable simple log mode (one-line summaries)
```

**Examples:**

```bash
# Start with redacted SQLite diagnostics and metadata-level debug logging
./claude-code-proxy -d

# Start with simple one-line summaries
./claude-code-proxy -s

# Combine flags
./claude-code-proxy -d -s
```

**Option 1: Use ccp wrapper (recommended)**

If you installed via `make install`, the `ccp` wrapper is already available:

```bash
# Use ccp instead of claude
ccp chat
ccp --version
ccp code /path/to/project
```

The `ccp` wrapper automatically:
- Starts the proxy daemon (if not running)
- Sets `ANTHROPIC_BASE_URL`
- Execs `claude` with your arguments

**No installation needed** - `ccp` is installed system-wide with `make install`.

**Option 2: Use with Claude Code directly**

```bash
# Start the proxy
./claude-code-proxy

# Configure Claude Code to use the proxy
export ANTHROPIC_BASE_URL=http://localhost:8082
claude chat
```

## Pattern-Based Routing

The proxy auto-detects Claude model names:

| Claude Model Pattern | Default OpenAI Model |
|---------------------|---------------------|
| `*opus*` | `gpt-5` |
| `*sonnet*` | `gpt-5` |
| `*haiku*` | `gpt-5-mini` |

Override with env vars:
```bash
ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5
ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5
ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5-mini
```

## Configuration Reference

**Required:**
- `OPENAI_API_KEY` - One API key (legacy single-key mode; not needed for Ollama/localhost)
- `OPENAI_API_KEYS` - Optional comma-separated API keys for manual selection; do not set it together with `OPENAI_API_KEY`
- `OPENAI_API_KEY_INDEX` - 1-based selected entry from `OPENAI_API_KEYS` (default: `1`); change it and restart to switch accounts. Logs and diagnostics expose only anonymous `key-1` / `key-2` labels.

**Optional - API Configuration:**
- `OPENAI_BASE_URL` - API base URL (default: `https://api.openai.com/v1`)
  - For OpenRouter: `https://openrouter.ai/api/v1`
  - For NewAPI: Your self-hosted OpenAI-compatible `/v1` endpoint
  - For Ollama: `http://localhost:11434/v1`
  - For other providers: Use their OpenAI-compatible endpoint
- `OPENAI_PROVIDER` - Optional explicit provider selection: `openrouter`, `openai`, `ollama`, `newapi`, or `generic`
  - Set `newapi` for a self-hosted NewAPI deployment to enable transparent effort forwarding

**Optional - Model Routing:**
- `ANTHROPIC_DEFAULT_OPUS_MODEL` - Override opus routing (default: `gpt-5`)
- `ANTHROPIC_DEFAULT_SONNET_MODEL` - Override sonnet routing (default: `gpt-5`)
- `ANTHROPIC_DEFAULT_HAIKU_MODEL` - Override haiku routing (default: `gpt-5-mini`)

Examples with OpenRouter:
```bash
ANTHROPIC_DEFAULT_SONNET_MODEL=x-ai/grok-code-fast-1
ANTHROPIC_DEFAULT_HAIKU_MODEL=google/gemini-2.5-flash
ANTHROPIC_DEFAULT_OPUS_MODEL=openai/gpt-5
```

**Optional - OpenRouter Specific:**
- `OPENROUTER_APP_NAME` - App name for OpenRouter dashboard tracking
- `OPENROUTER_APP_URL` - App URL for better rate limits (higher quotas)

**Optional - Playground Features:**
- `OCR_MODEL` - Optional vision-capable Chat Completions model for Playground OCR; uploaded JPEG/PNG/WebP image bytes are sent upstream for that request and are not persisted by the proxy
- `PPT_MODEL` - Optional Chat Completions model override for validated Playground PPT outlines
- `IMAGE_API_URL`, `IMAGE_API_KEY`, `IMAGE_MODEL` - Required together to enable independent Playground image generation; the dedicated image key never falls back to `OPENAI_API_KEY`

**Optional - Diagnostics:**
- `DIAGNOSTICS_ENABLED` - Enable local redacted SQLite diagnostics (`true`, `1`, or `yes`; default: `false`)
- `DIAGNOSTICS_CAPTURE_CONTENT` - Explicitly retain bounded local sent/received payload snapshots (default: `false`; requires `DIAGNOSTICS_ENABLED=true`)
- `DIAGNOSTICS_CONTENT_RETENTION` - Content snapshot retention as a positive Go duration no longer than diagnostics retention (default: `1h`)
- `DIAGNOSTICS_DB_PATH` - Override the SQLite database path
- `DIAGNOSTICS_RETENTION` - Retention as a positive Go duration (default: `72h`; cleanup runs at startup)
- `DIAGNOSTICS_BUSY_TIMEOUT` - SQLite busy timeout (default: `5s`)

**Optional - Prompt Archive:**
- `PROMPT_ARCHIVE_ENABLED` - Enable independent Prompt 存档 database used by `/prompts`
- `PROMPT_ARCHIVE_DB_PATH` - Override the Prompt Archive SQLite path
- `PROMPT_ARCHIVE_RETENTION` - Prompt Archive retention as a positive Go duration (default: `168h`)

**Optional - Security:**
- `ANTHROPIC_API_KEY` - Client API key validation (optional)
  - If set, clients must provide this exact key
  - Leave unset to disable validation

**Optional - Server Settings:**
- `HOST` - Server host (default: `0.0.0.0`)
- `PORT` - Server port (default: `8082`)
- `PASSTHROUGH_MODE` - Direct proxy to Anthropic API (default: `false`)

## Local Web UI

The proxy now ships with loopback-only browser pages:

- `/` - dashboard
- `/debug/logs` - diagnostics viewer
- `/monitor` - traffic monitor
- `/prompts` - prompt archive browser
- `/playground` - interactive workbench

### Playground layout

The Playground page is structured for long-term expansion:

- **Chat**: streaming response, stop generation, reasoning/text split, manual model entry
- **Image workflow**: optional independent image API with bounded local previews (requires `IMAGE_API_URL`, `IMAGE_API_KEY`, and `IMAGE_MODEL`; never falls back to the chat key)
- **OCR**: JPEG/PNG/WebP upload to a configured vision-capable chat model (`OCR_MODEL` optional); image bytes are not persisted by the proxy
- **PPT**: validated `ppt-outline/v1` deck preview plus Markdown/HTML export (no `.pptx` export)
- **Providers / Keys**: UI scaffold for multiple providers, base URLs, and key labels
- **Templates**: persisted task presets with workflow metadata

### Prompt archive

Prompt Archive is independent from diagnostics:

- Enable with `PROMPT_ARCHIVE_ENABLED=true`
- Use `PROMPT_ARCHIVE_DB_PATH` to change the database location
- Use `PROMPT_ARCHIVE_RETENTION` to control retention

It stores redacted request summaries and metadata for search, export, and debugging.

## Build for Distribution

```bash
# Build for all platforms
make build-all

# Output:
# dist/claude-code-proxy-darwin-amd64
# dist/claude-code-proxy-darwin-arm64
# dist/claude-code-proxy-linux-amd64
# dist/claude-code-proxy-linux-arm64
# dist/claude-code-proxy-windows-amd64.exe
```

## Project Structure

```
proxy/
├── cmd/
│   └── claude-code-proxy/
│       └── main.go           # Entry point
├── internal/
│   ├── config/               # Config loading
│   ├── daemon/               # Process management
│   ├── server/               # HTTP server (Fiber)
│   └── converter/            # Claude ↔ OpenAI conversion
├── pkg/
│   └── models/               # Shared types
├── scripts/
│   └── ccp                   # Shell wrapper
└── Makefile                  # Build automation
```

## Supported Claude Code Features

The proxy fully supports all Claude Code features:

- **Tool Calling** - Complete support for all Claude Code tools
  - File operations: `read`, `write`, `edit`
  - Search operations: `glob`, `grep`
  - Shell execution: `bash`
  - Task management: `todowrite`, `todoread`
  - And all other Claude Code tools

- **Extended Thinking** - Proper thinking block support
  - Thinking blocks are properly formatted and hidden in Claude Code UI
  - Shows "Thought for Xs" indicator instead of full content
  - Can be revealed with Ctrl+O in Claude Code
  - For upstreams that provide reasoning text, it is converted to Claude thinking blocks
  - Compatible provider signatures are forwarded when supplied; the proxy never fabricates signatures

- **Streaming** - Real-time streaming responses
  - Proper SSE (Server-Sent Events) formatting
  - Accurate token usage tracking
  - Low latency streaming from backend models

- **Token Tracking** - Full usage metrics
  - Input tokens counted accurately
  - Output tokens tracked in real-time
  - Cache metrics supported (when using Anthropic backend)

## Development

```bash
# Run in dev mode
go run ./cmd/claude-code-proxy

# Run tests
go test ./...
# Or with verbose output
go test -v ./internal/converter

# Run specific test
go test -v ./internal/converter -run TestConvertMessagesWithComplexContent

# Format code
go fmt ./...

# Lint (requires golangci-lint)
golangci-lint run
```

## Testing

### Unit Tests

The project includes comprehensive unit tests:

```bash
# Run all tests
go test ./...

# Run converter tests (includes tool calling tests)
go test -v ./internal/converter

# Run with coverage
go test -cover ./...
```

### Manual Testing

Test the proxy with Claude Code CLI:

```bash
# Start proxy in background
./claude-code-proxy -s &

# Test with different model tiers
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model opus -p "hi"
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model sonnet -p "hi"
ANTHROPIC_BASE_URL=http://localhost:8082 claude --model haiku -p "hi"

# Check proxy logs
./claude-code-proxy status
tail -f /tmp/claude-code-proxy.log

# Stop proxy
./claude-code-proxy stop
```

See [CLAUDE.md](CLAUDE.md#manual-testing) for detailed testing instructions including tool calling, streaming, and provider-specific tests.

## How It Works

1. **Request Flow**:
   - Claude Code sends Claude API format request to proxy
   - Proxy converts Claude format → OpenAI format
   - Proxy routes to OpenRouter/OpenAI/other provider
   - Provider returns OpenAI format response
   - Proxy converts back to Claude format
   - Claude Code receives properly formatted response

2. **Format Conversion**:
   - Claude's `tool_use` blocks → OpenAI's `tool_calls` format
   - OpenAI's `reasoning_details` → Claude's `thinking` blocks
   - Maintains proper tool_use ↔ tool_result correspondence
   - Preserves all metadata and signatures

3. **Streaming**:
   - Converts OpenAI SSE chunks → Claude SSE events
   - Generates proper event sequence (message_start, content_block_start, deltas, etc.)
   - Tracks content block indices for proper Claude Code rendering

## Adaptive Per-Model Detection

The proxy uses a fully adaptive system that automatically learns what parameters each model supports, eliminating the need for hardcoded model patterns or provider-specific configuration.

### How It Works

**Philosophy:** Support all provider quirks automatically - never burden users with configurations they don't understand.

1. **First Request** (Cache Miss):
   ```
   [DEBUG] Cache MISS: gpt-5 → will auto-detect (try max_completion_tokens)
   ```
   - Proxy tries sending `max_completion_tokens` (correct for reasoning models)
   - If provider returns "unsupported parameter" error, automatically retries without it
   - Result is cached per `(provider, model)` combination

2. **Subsequent Requests** (Cache Hit):
   ```
   [DEBUG] Cache HIT: gpt-5 → max_completion_tokens=true
   ```
   - Proxy uses cached knowledge immediately
   - No trial-and-error needed
   - Instant parameter selection

### Benefits

- **Zero Configuration** - No need to know which parameters each provider supports
- **Future-Proof** - Works with any new model/provider without code changes
- **Fast** - Only 1-2 second first request penalty, instant thereafter
- **Provider-Agnostic** - Automatically adapts to OpenRouter, OpenAI Direct, Ollama, OpenWebUI, or any OpenAI-compatible provider
- **Per-Model Granularity** - Same model name on different providers cached separately

### Cache Details

**What's Cached:**
```go
CacheKey{
    BaseURL: "https://gpt.erst.dk/api",  // Provider
    Model:   "gpt-5"                      // Model name
}
→ ModelCapabilities{
    UsesMaxCompletionTokens: false,       // Learned capability
    LastChecked:             time.Now()   // Timestamp
}
```

**Cache Scope:**
- In-memory only (cleared on proxy restart)
- Thread-safe (protected by `sync.RWMutex`)
- Per (provider, model) combination
- Visible in debug logs (`-d` flag)

### Example: OpenWebUI

When using OpenWebUI (which has a quirk with `max_completion_tokens`):

| Request | What Happens | Duration |
|---------|--------------|----------|
| 1st | Try max_completion_tokens → Error → Retry without it | ~2 seconds |
| 2nd+ | Use cached knowledge (no retry) | < 100ms |

**No configuration needed** - the proxy learns and adapts automatically.

### Debug Logging and Diagnostics

Enable debug mode to see capability-cache activity and other operational metadata while recording redacted request diagnostics in SQLite:

```bash
./claude-code-proxy -d -s

# Logs show metadata such as:
# [DEBUG] Cache MISS: gpt-5 → will auto-detect (try max_completion_tokens)
# [DEBUG] Cached: model gpt-5 supports max_completion_tokens
# [DEBUG] Cache HIT: gpt-5 → max_completion_tokens=true
```

`-d` does not print full request and response bodies. It enables the local diagnostics database and the loopback-only viewer at `http://127.0.0.1:8082/debug/logs`. To enable diagnostics without verbose console debug messages, set `DIAGNOSTICS_ENABLED=true`. Records are retained for `72h` by default; override this with `DIAGNOSTICS_RETENTION` and override the database location with `DIAGNOSTICS_DB_PATH`.

If startup reports an incompatible diagnostics content schema, foreign-key integrity failure, or unexpected trigger, the existing SQLite database does not match the supported diagnostics contract. The proxy will not delete or rebuild it automatically: back it up, inspect its schema/triggers, or point `DIAGNOSTICS_DB_PATH` to a new database. This is different from `context_window_exceeded`, which is the classified outcome of an upstream model rejecting an over-limit request rather than a diagnostics persistence failure.

## License

MIT
