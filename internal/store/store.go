package store

import (
	"encoding/json"
	"os"
	"path/filepath"

	"monya/internal/api"
)

func historyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "share", "monya", "history")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// LoadHistory reads the message history for an agent from disk.
// Returns nil, nil if no history exists yet.
func LoadHistory(agent string) ([]api.Message, error) {
	dir, err := historyDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, agent+".json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []api.Message
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// SaveHistory writes the message history for an agent to disk.
func SaveHistory(agent string, msgs []api.Message) error {
	dir, err := historyDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, agent+".json")
	b, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
