package config

import (
	"strings"
	"testing"
)

func TestLoadValidatesStreamCompression(t *testing.T) {
	t.Setenv("GROK_STREAM_COMPRESSION", "brotli")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROK_STREAM_COMPRESSION") {
		t.Fatalf("Load() error = %v, want stream compression validation error", err)
	}
}

func TestLoadAdminKey(t *testing.T) {
	t.Setenv("GROK_ADMIN_KEY", "  admin-secret  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminKey != "admin-secret" {
		t.Fatalf("AdminKey = %q", cfg.AdminKey)
	}
}
