package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"monya/internal/api"
)

type Agent struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

type Chat struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	AgentID        string    `json:"agent_id"`
	Thinking       bool      `json:"thinking"`
	TrustExecution bool      `json:"trust_execution"`
	UpdatedAt      time.Time `json:"updated_at"`
	LegacyName     string    `json:"legacy_name,omitempty"`
}

type Workspace struct {
	Agents        []Agent  `json:"agents"`
	Chats         []Chat   `json:"chats"`
	LegacyImports []string `json:"legacy_imports,omitempty"`
}

func NewID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("store: generate ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func dataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "monya"), nil
}

func historyDir() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "history"), nil
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("store: create directory: %w", err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".monya-*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("store: replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

func LoadWorkspace() (Workspace, error) {
	dir, err := dataDir()
	if err != nil {
		return Workspace{}, err
	}
	path := filepath.Join(dir, "workspace.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Workspace{}, nil
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("store: read workspace: %w", err)
	}
	var workspace Workspace
	if err := json.Unmarshal(data, &workspace); err != nil {
		return Workspace{}, fmt.Errorf("store: decode workspace: %w", err)
	}
	return workspace, nil
}

func SaveWorkspace(workspace Workspace) error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "workspace.json"), workspace)
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func chatHistoryPath(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("store: invalid chat ID")
	}
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "chats", id+".json"), nil
}

func LoadChatHistory(id string) ([]api.Message, error) {
	path, err := chatHistoryPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read chat history: %w", err)
	}
	var messages []api.Message
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("store: decode chat history: %w", err)
	}
	return messages, nil
}

func SaveChatHistory(id string, messages []api.Message) error {
	path, err := chatHistoryPath(id)
	if err != nil {
		return err
	}
	return writeJSON(path, messages)
}

func DeleteChatHistory(id string) error {
	path, err := chatHistoryPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete chat history: %w", err)
	}
	return nil
}

func MigrateLegacyHistories(workspace *Workspace) (bool, error) {
	dir, err := historyDir()
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: list legacy histories: %w", err)
	}

	changed := false
	imported := make(map[string]bool, len(workspace.LegacyImports)+len(workspace.Chats))
	for _, name := range workspace.LegacyImports {
		imported[name] = true
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		for _, chat := range workspace.Chats {
			if chat.LegacyName == name {
				if !imported[name] {
					workspace.LegacyImports = append(workspace.LegacyImports, name)
					changed = true
				}
				imported[name] = true
				break
			}
		}
		if imported[name] {
			continue
		}

		messages, err := loadLegacyHistory(filepath.Join(dir, entry.Name()))
		if err != nil {
			return changed, err
		}
		agentID := ""
		for _, agent := range workspace.Agents {
			if agent.Name == name {
				agentID = agent.ID
				break
			}
		}
		if agentID == "" {
			agentID, err = NewID()
			if err != nil {
				return changed, err
			}
			workspace.Agents = append(workspace.Agents, Agent{ID: agentID, Name: name})
		}
		chatID, err := NewID()
		if err != nil {
			return changed, err
		}
		chat := Chat{
			ID:         chatID,
			Title:      name,
			AgentID:    agentID,
			Thinking:   true,
			UpdatedAt:  time.Now(),
			LegacyName: name,
		}
		if err := SaveChatHistory(chatID, messages); err != nil {
			return changed, err
		}
		workspace.Chats = append(workspace.Chats, chat)
		workspace.LegacyImports = append(workspace.LegacyImports, name)
		imported[name] = true
		changed = true
	}
	return changed, nil
}

func LoadHistory(agent string) ([]api.Message, error) {
	dir, err := historyDir()
	if err != nil {
		return nil, err
	}
	return loadLegacyHistory(filepath.Join(dir, filepath.Base(agent)+".json"))
}

func loadLegacyHistory(path string) ([]api.Message, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read legacy history: %w", err)
	}
	var messages []api.Message
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("store: decode legacy history %s: %w", filepath.Base(path), err)
	}
	return messages, nil
}

func SaveHistory(agent string, messages []api.Message) error {
	dir, err := historyDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, filepath.Base(agent)+".json"), messages)
}
