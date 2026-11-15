package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"monya/internal/api"
	"monya/internal/config"
	"monya/internal/ratelimit"
	"monya/internal/tui"
)

const version = "0.0.1-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to config.toml (default: ~/.config/monya/config.toml)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	var (
		cfg *config.Config
		err error
	)
	if *configPath != "" {
		cfg, err = config.LoadFrom(*configPath)
	} else {
		cfg, err = config.Load()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "monya: config error: %v\n", err)
		os.Exit(1)
	}

	tr := ratelimit.NewTransport(
		http.DefaultTransport,
		cfg.RateLimit.RequestsPerSecond,
		cfg.RateLimit.Burst,
		cfg.RateLimit.MaxConcurrent,
	)
	httpClient := &http.Client{Transport: tr}
	client := api.NewClient(cfg.APIURL, cfg.DefaultAgent).WithHTTPClient(httpClient)

	p := tea.NewProgram(tui.New(client, cfg), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "monya: %v\n", err)
		os.Exit(1)
	}
}
