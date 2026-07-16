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

const healthURL = "http://localhost:8082/health"

var pidFile = defaultPIDFile()

func defaultPIDFile() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	return filepath.Join(cacheDir, "claude-code-proxy", "claude-code-proxy.pid")
}

// IsRunning checks if the proxy daemon is running
func IsRunning() bool {
	// Try health check first
	resp, err := http.Get(healthURL)
	if err == nil {
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}

	// Fallback: check PID file
	return isProcessRunning()
}

// Start daemonizes the current process
func Start() error {
	// Already running check
	if IsRunning() {
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

// Stop stops the running daemon
func Stop() {
	if !IsRunning() {
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

// Status prints the current daemon status
func Status() {
	if IsRunning() {
		pid, _ := readPID()
		fmt.Printf("✅ Proxy is running (PID: %d)\n", pid)
		fmt.Printf("   Health endpoint: %s\n", healthURL)
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
