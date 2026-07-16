// Package daemon handles background process management for the proxy server.
//
// It manages PID file creation/deletion, process health checks, and provides functions
// to start, stop, and check the status of the proxy daemon. The daemon runs in the
// background and can be controlled via the CLI (start, stop, status commands).
package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

const defaultPort = "8082"

var pidFile = defaultPIDFile()

func defaultPIDFile() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	return filepath.Join(cacheDir, "claude-code-proxy", "claude-code-proxy.pid")
}

// IsRunning checks if the default proxy daemon is running.
func IsRunning() bool {
	return IsRunningAt("0.0.0.0", defaultPort)
}

// IsRunningAt checks if the proxy daemon is running on the configured host and port.
func IsRunningAt(host, port string) bool {
	resp, err := http.Get(healthURL(host, port))
	if err == nil {
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	return isProcessRunning()
}

func healthURL(host, port string) string {
	if port == "" {
		port = defaultPort
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port + "/health"
}

// Start records the current proxy process after confirming no default instance is running.
func Start() error {
	return StartAt("0.0.0.0", defaultPort)
}

// StartAt records the current proxy process after confirming no configured instance is running.
func StartAt(host, port string) error {
	if IsRunningAt(host, port) {
		return fmt.Errorf("proxy is already running")
	}

	// Clean up stale PID file
	cleanupPID()

	// Write PID file
	if err := writePID(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	fmt.Println("🚀 Starting Claude Code Proxy daemon...")
	return nil
}

// Stop stops the default proxy daemon.
func Stop() {
	StopAt("0.0.0.0", defaultPort)
}

// StopAt stops the configured proxy daemon.
func StopAt(host, port string) {
	if !IsRunningAt(host, port) {
		fmt.Println("Proxy is not running")
		return
	}

	pid, err := readPID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading PID: %v\n", err)
		return
	}

	if err := terminateProcess(pid); err != nil {
		fmt.Fprintf(os.Stderr, "Error stopping process: %v\n", err)
		return
	}

	cleanupPID()
	fmt.Println("✅ Proxy stopped")
}

// Status prints the default daemon status.
func Status() {
	StatusAt("0.0.0.0", defaultPort)
}

// StatusAt prints the current daemon status for the configured host and port.
func StatusAt(host, port string) {
	if IsRunningAt(host, port) {
		pid, _ := readPID()
		fmt.Printf("✅ Proxy is running (PID: %d)\n", pid)
		fmt.Printf("   Health endpoint: %s\n", healthURL(host, port))
	} else {
		fmt.Println("❌ Proxy is not running")
	}
}

// Helper functions

func writePID() error {
	if err := os.MkdirAll(filepath.Dir(pidFile), 0700); err != nil {
		return err
	}
	pid := os.Getpid()
	return os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0644)
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func cleanupPID() {
	_ = os.Remove(pidFile) // Ignore error - cleanup is best-effort
}

func isProcessRunning() bool {
	pid, err := readPID()
	if err != nil {
		return false
	}
	return processExists(pid)
}

// Cleanup should be called on shutdown
func Cleanup() {
	cleanupPID()
}
