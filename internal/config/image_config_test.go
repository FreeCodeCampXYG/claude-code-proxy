package config

import "testing"

func TestImageFeatureConfig(t *testing.T) {
	cfg := &Config{ImageAPIURL: "https://images.example.com/v1/", ImageAPIKey: "image-key", ImageModel: "image-test"}
	if err := validateImageConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ImageGenerationsURL(); got != "https://images.example.com/v1/images/generations" {
		t.Fatalf("ImageGenerationsURL() = %q", got)
	}
	cfg.ImageAPIURL = "https://images.example.com/v1/images/generations?api-version=2025-01-01"
	if got := cfg.ImageGenerationsURL(); got != cfg.ImageAPIURL {
		t.Fatalf("full image endpoint changed to %q", got)
	}
	cfg.ImageAPIURL = "https://images.example.com/v1?api-version=2025-01-01"
	if got := cfg.ImageGenerationsURL(); got != "https://images.example.com/v1/images/generations?api-version=2025-01-01" {
		t.Fatalf("query image endpoint = %q", got)
	}
	for _, invalid := range []*Config{
		{ImageAPIURL: "https://images.example.com/v1"},
		{ImageAPIURL: "ftp://images.example.com", ImageAPIKey: "key", ImageModel: "model"},
		{ImageAPIURL: "https://user:pass@images.example.com/v1", ImageAPIKey: "key", ImageModel: "model"},
	} {
		if err := validateImageConfig(invalid); err == nil {
			t.Fatalf("validateImageConfig(%#v) unexpectedly succeeded", invalid)
		}
	}
}
