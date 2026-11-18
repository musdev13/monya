package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func approvedCommandsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "monya", "approved_commands.json"), nil
}

func LoadApprovedCommands() ([]string, error) {
	path, err := approvedCommandsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read approved commands: %w", err)
	}
	var commands []string
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil, fmt.Errorf("config: decode approved commands: %w", err)
	}
	return commands, nil
}

func SaveApprovedCommands(commands []string) error {
	path, err := approvedCommandsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create directory for approved commands: %w", err)
	}
	commands = append([]string(nil), commands...)
	sort.Strings(commands)
	data, err := json.MarshalIndent(commands, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode approved commands: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".approved-commands-*.tmp")
	if err != nil {
		return fmt.Errorf("config: create approved commands file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write approved commands: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close approved commands: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: replace approved commands: %w", err)
	}
	return nil
}
