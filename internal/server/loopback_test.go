package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

func startLoopbackTestServer(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Listener(ln)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.ShutdownWithContext(ctx); err != nil {
			t.Errorf("shutdown loopback test server: %v", err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("loopback test server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("loopback test server did not stop")
		}
	})

	return "http://" + ln.Addr().String()
}

func getLoopbackTest(t *testing.T, baseURL, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
