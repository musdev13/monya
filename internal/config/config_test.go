package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFrom_MissingFile_ReturnsDefaults(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := Default()
	if cfg.APIURL != def.APIURL {
		t.Errorf("APIURL = %q, want %q", cfg.APIURL, def.APIURL)
	}
	if cfg.DefaultModel != def.DefaultModel {
		t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, def.DefaultModel)
	}
}

func TestLoadFrom_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
api_url = "http://example.com:1234"
rate_limit = { requests_per_second = 2.0 }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.APIURL != "http://example.com:1234" {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
	if cfg.RateLimit.RequestsPerSecond != 2.0 {
		t.Errorf("RPS = %v", cfg.RateLimit.RequestsPerSecond)
	}
	// Unspecified fields must keep defaults
	def := Default()
	if cfg.DefaultModel != def.DefaultModel {
		t.Errorf("DefaultModel = %q, want default %q", cfg.DefaultModel, def.DefaultModel)
	}
	if cfg.RateLimit.MaxConcurrent != def.RateLimit.MaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want default %d", cfg.RateLimit.MaxConcurrent, def.RateLimit.MaxConcurrent)
	}
}

func TestLoadFrom_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte(`this is not = valid = toml`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected error on invalid TOML")
	}
}

func TestLoadFrom_SanityFallbacks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird.toml")
	if err := os.WriteFile(path, []byte(`
api_url = ""
rate_limit = { requests_per_second = -5, max_concurrent = 0, burst = 0 }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	def := Default()
	if cfg.APIURL != def.APIURL {
		t.Errorf("APIURL fallback failed: %q", cfg.APIURL)
	}
	if cfg.RateLimit.RequestsPerSecond != def.RateLimit.RequestsPerSecond {
		t.Errorf("RPS fallback failed: %v", cfg.RateLimit.RequestsPerSecond)
	}
	if cfg.RateLimit.MaxConcurrent != def.RateLimit.MaxConcurrent {
		t.Errorf("MaxConcurrent fallback failed: %d", cfg.RateLimit.MaxConcurrent)
	}
}
