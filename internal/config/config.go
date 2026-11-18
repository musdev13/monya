package config

import (
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// RateLimit configures internal/ratelimit.Transport.
type RateLimit struct {
	RequestsPerSecond float64 `toml:"requests_per_second"`
	Burst             int     `toml:"burst"`
	MaxConcurrent     int     `toml:"max_concurrent"`
}

// Config holds all tunable parameters of monya.
type Config struct {
	APIURL       string    `toml:"api_url"`
	DefaultModel string    `toml:"default_model"`
	DefaultAgent string    `toml:"default_agent"`
	RateLimit    RateLimit `toml:"rate_limit"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		APIURL:       "http://127.0.0.1:8080",
		DefaultModel: "/home/idk/Models/Qwen3.5-4B-Q4_K_M.gguf",
		DefaultAgent: "dev-agent",
		RateLimit: RateLimit{
			RequestsPerSecond: 0.5, // 1 request per 2 seconds by default
			Burst:             1,
			MaxConcurrent:     1,
		},
	}
}

// Load reads the config from ~/.config/monya/config.toml.
// If the file does not exist, it returns Default().
func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Default(), nil
	}
	return LoadFrom(filepath.Join(home, ".config", "monya", "config.toml"))
}

// LoadFrom reads the config from an explicit path.
// If the file does not exist, it returns Default().
// Fields present in the file override defaults; missing fields keep default values.
func LoadFrom(path string) (*Config, error) {
	cfg := Default()

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := toml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}

	// Sanity fallbacks
	if cfg.APIURL == "" {
		cfg.APIURL = Default().APIURL
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = Default().DefaultAgent
	}
	if cfg.RateLimit.RequestsPerSecond <= 0 {
		cfg.RateLimit.RequestsPerSecond = Default().RateLimit.RequestsPerSecond
	}
	if cfg.RateLimit.MaxConcurrent <= 0 {
		cfg.RateLimit.MaxConcurrent = Default().RateLimit.MaxConcurrent
	}
	if cfg.RateLimit.Burst <= 0 {
		cfg.RateLimit.Burst = Default().RateLimit.Burst
	}

	return cfg, nil
}
