package main

import (
	"fmt"
	"os"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/daemon"
	"github.com/claude-code-proxy/proxy/internal/server"
)

func main() {
	// Parse command and flags
	debug := false
	simpleLog := false
	command := ""

	if len(os.Args) > 1 {
		for i := 1; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "-d", "--debug":
				debug = true
			case "-s", "--simple":
				simpleLog = true
			case "stop", "status", "version", "help", "-h", "--help":
				command = arg
			}
		}

		// Handle commands
		host, port := daemonSettings()
		switch command {
		case "stop":
			daemon.StopAt(host, port)
			return
		case "status":
			daemon.StatusAt(host, port)
			return
		case "version":
			fmt.Println("claude-code-proxy " + server.ProxyVersion)
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	// Load configuration with debug mode
	var cfg *config.Config
	var err error
	if debug {
		cfg, err = config.LoadWithDebug(true)
		fmt.Println("🐛 Diagnostic mode enabled - redacted requests are stored locally; content capture remains opt-in")
	} else {
		cfg, err = config.Load()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Enable simple logging if requested
	if simpleLog {
		cfg.SimpleLog = true
		fmt.Println("📊 Simple log mode enabled - one-line summaries per request")
	}

	// Check if already running
	if daemon.IsRunningAt(cfg.Host, cfg.Port) {
		if debug {
			fmt.Printf("Proxy is already running; diagnostics cannot be enabled on the existing process. Run %s stop, then restart with -d.\n", os.Args[0])
		} else {
			fmt.Println("Proxy is already running")
		}
		return
	}

	// Record this process for status and stop commands.
	if err := daemon.StartAt(cfg.Host, cfg.Port); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n", err)
		os.Exit(1)
	}

	// Start HTTP server (blocks)
	// Note: No need to pre-fetch reasoning models - adaptive per-model detection
	// handles all models automatically through retry mechanism
	if err := server.Start(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting server: %v\n", err)
		os.Exit(1)
	}
}

func daemonSettings() (string, string) {
	if cfg, err := config.Load(); err == nil {
		return cfg.Host, cfg.Port
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	return host, port
}

func printHelp() {
	fmt.Println(`Claude Code Proxy - OpenAI API proxy for Claude Code

Usage:
  claude-code-proxy [-d|--debug] [-s|--simple]  Start the proxy daemon
  claude-code-proxy stop                        Stop the proxy daemon
  claude-code-proxy status                      Check if proxy is running
  claude-code-proxy version                     Show version
  claude-code-proxy help                        Show this help

Flags:
  -d, --debug     Enable local redacted diagnostics (SQLite + /debug/logs)
  -s, --simple    Enable simple log mode (one-line summary per request)

Configuration:
  Config file locations (checked in order):
    1. ./.env
    2. ~/.claude/proxy.env
    3. ~/.claude-code-proxy

  Required:
    OPENAI_API_KEY         One upstream API key (legacy single-key mode)
    OPENAI_API_KEYS        Comma-separated keys for manual selection
    OPENAI_API_KEY_INDEX   1-based selected key index (default: 1)

  Optional:
    ANTHROPIC_DEFAULT_OPUS_MODEL    Override opus routing
    ANTHROPIC_DEFAULT_SONNET_MODEL  Override sonnet routing
    ANTHROPIC_DEFAULT_HAIKU_MODEL   Override haiku routing
    OPENAI_BASE_URL                 OpenAI API base URL
    OPENAI_PROVIDER                 auto/openai/openrouter/ollama/newapi/generic
    DIAGNOSTICS_ENABLED             Store redacted diagnostics in SQLite
    DIAGNOSTICS_CAPTURE_CONTENT     Store bounded local payload snapshots (default: false)
    DIAGNOSTICS_CONTENT_RETENTION   Payload snapshot retention (default: 1h)
    DIAGNOSTICS_DB_PATH             Override diagnostics database path
    DIAGNOSTICS_RETENTION           Retention duration (default: 72h)
    HOST                            Server host (default: 0.0.0.0)
    PORT                            Server port (default: 8082)

Examples:
  # Start proxy
  claude-code-proxy

  # Use with Claude Code (via ccp wrapper)
  ccp chat

  # Or manually
  ANTHROPIC_BASE_URL=http://localhost:8082 claude chat`)
}
