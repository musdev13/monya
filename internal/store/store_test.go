package store

import (
	"os"
	"path/filepath"
	"testing"

	"monya/internal/api"
)

func TestWorkspaceAndChatHistoryPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	workspace := Workspace{
		Agents: []Agent{{ID: "agent-1", Name: "Helper", SystemPrompt: "Be helpful."}},
		Chats:  []Chat{{ID: id, Title: "First chat", AgentID: "agent-1", Thinking: false}},
	}
	if err := SaveWorkspace(workspace); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	messages := []api.Message{{Role: "user", Content: "hello"}}
	if err := SaveChatHistory(id, messages); err != nil {
		t.Fatalf("SaveChatHistory: %v", err)
	}

	gotWorkspace, err := LoadWorkspace()
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if len(gotWorkspace.Agents) != 1 || gotWorkspace.Agents[0].SystemPrompt != "Be helpful." {
		t.Fatalf("agents did not persist: %+v", gotWorkspace.Agents)
	}
	if len(gotWorkspace.Chats) != 1 || gotWorkspace.Chats[0].Thinking {
		t.Fatalf("chat settings did not persist: %+v", gotWorkspace.Chats)
	}
	gotMessages, err := LoadChatHistory(id)
	if err != nil {
		t.Fatalf("LoadChatHistory: %v", err)
	}
	if len(gotMessages) != 1 || gotMessages[0].Content != "hello" {
		t.Fatalf("chat history did not persist: %+v", gotMessages)
	}
	if err := DeleteChatHistory(id); err != nil {
		t.Fatalf("DeleteChatHistory: %v", err)
	}
	gotMessages, err = LoadChatHistory(id)
	if err != nil {
		t.Fatalf("LoadChatHistory after delete: %v", err)
	}
	if gotMessages != nil {
		t.Fatalf("chat history still exists after delete: %+v", gotMessages)
	}
}

func TestMigrateLegacyHistoriesIsIdempotentAndKeepsSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyDir := filepath.Join(home, ".local", "share", "monya", "history")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(legacyDir, "old-agent.json")
	if err := os.WriteFile(legacyPath, []byte(`[{"role":"user","content":"saved"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	var workspace Workspace
	changed, err := MigrateLegacyHistories(&workspace)
	if err != nil {
		t.Fatalf("MigrateLegacyHistories: %v", err)
	}
	if !changed || len(workspace.Agents) != 1 || len(workspace.Chats) != 1 {
		t.Fatalf("migration result: changed=%v workspace=%+v", changed, workspace)
	}
	if workspace.Agents[0].Name != "old-agent" || workspace.Chats[0].LegacyName != "old-agent" {
		t.Fatalf("legacy identity not preserved: %+v", workspace)
	}
	messages, err := LoadChatHistory(workspace.Chats[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != "saved" {
		t.Fatalf("migrated history = %+v", messages)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy source was removed: %v", err)
	}

	workspace.Chats = nil
	changed, err = MigrateLegacyHistories(&workspace)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(workspace.Chats) != 0 {
		t.Fatalf("deleted legacy chat was re-imported: changed=%v chats=%+v", changed, workspace.Chats)
	}
}
