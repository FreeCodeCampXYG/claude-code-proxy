package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
)

func buildUpstreamRequest(req *http.Request, cfg *config.Config, requestID string) {
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	if !cfg.IsLocalhost() {
		req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(req, cfg)
	}
}

func newStreamingUpstreamClient(cfg *config.Config) (*http.Client, error) {
	transport, err := upstreamTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}

func newUnaryUpstreamClient(cfg *config.Config) (*http.Client, error) {
	transport, err := upstreamTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 90 * time.Second, Transport: transport}, nil
}

func upstreamTransport(cfg *config.Config) (http.RoundTripper, error) {
	base := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if cfg == nil {
		return base, nil
	}
	proxyCfg := config.ProxyConfig{Enabled: cfg.UpstreamProxyEnabled, Type: cfg.UpstreamProxyType, Address: cfg.UpstreamProxyAddr, Username: cfg.UpstreamProxyUser, Password: cfg.UpstreamProxyPass}
	proxyCfg = config.NormalizeProxyConfig(proxyCfg)
	if !proxyCfg.Enabled {
		if cfg.ProxyRuntime != nil {
			proxyCfg = cfg.ProxyRuntime.Snapshot()
		}
	}
	if !proxyCfg.Enabled {
		return base, nil
	}
	if proxyCfg.Type != "http" {
		return nil, errors.New("socks5 proxy support is not enabled yet")
	}
	httpURL, err := url.Parse("http://" + proxyCfg.Address)
	if err != nil {
		return nil, fmt.Errorf("parse upstream proxy address: %w", err)
	}
	base.Proxy = http.ProxyURL(httpURL)
	return base, nil
}
