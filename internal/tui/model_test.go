package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"monya/internal/api"
	"monya/internal/config"
	"monya/internal/store"
)

func TestWindowSizeClampsViewportForShortTerminal(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())

	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 7})

	if m.viewport.Width < 1 || m.viewport.Height < 1 {
		t.Fatalf("viewport size = %dx%d, want positive dimensions", m.viewport.Width, m.viewport.Height)
	}
}

func TestLoadWorkspacePersistsDefaultAgentAndChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	msg := loadWorkspace("local-agent")()
	loaded, ok := msg.(workspaceLoadedMsg)
	if !ok {
		t.Fatalf("loadWorkspace returned %T, want workspaceLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("loadWorkspace error: %v", loaded.err)
	}
	if len(loaded.workspace.Agents) != 1 || loaded.workspace.Agents[0].Name != "local-agent" {
		t.Fatalf("agents = %+v, want local-agent", loaded.workspace.Agents)
	}
	if len(loaded.workspace.Chats) != 1 {
		t.Fatalf("chats = %+v, want one default chat", loaded.workspace.Chats)
	}
	second := loadWorkspace("local-agent")().(workspaceLoadedMsg)
	if second.err != nil {
		t.Fatalf("second load error: %v", second.err)
	}
	if len(second.workspace.Agents) != 1 || second.workspace.Agents[0].ID != loaded.workspace.Agents[0].ID {
		t.Fatalf("agent list did not persist: %+v", second.workspace.Agents)
	}
	if len(second.workspace.Chats) != 1 || second.workspace.Chats[0].ID != loaded.workspace.Chats[0].ID {
		t.Fatalf("chat list did not persist: %+v", second.workspace.Chats)
	}

}

func TestThinkingTogglePersistsForCurrentChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{{ID: "chat-1", Title: "Test", AgentID: "agent-1", Thinking: true}},
	}
	m.activeChat = m.workspace.Chats[0]
	m.activeAgent = m.workspace.Agents[0]

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyF3})

	if m.activeChat.Thinking {
		t.Fatal("thinking should be disabled")
	}
	workspace, err := store.LoadWorkspace()
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if len(workspace.Chats) != 1 || workspace.Chats[0].Thinking {
		t.Fatalf("thinking mode was not persisted: %+v", workspace.Chats)
	}
}

func TestChatRequestUsesAgentPromptAndSavedThinkingMode(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.activeChat = store.Chat{ID: "chat-1", Thinking: false}
	m.activeAgent = store.Agent{Name: "Reviewer", SystemPrompt: "Review carefully."}
	m.history = []api.Message{{Role: "user", Content: "Check this."}}

	req := m.chatRequest()
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "Review carefully." {
		t.Fatalf("system prompt missing from request: %+v", req.Messages)
	}
	if req.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("thinking mode = %v, want false", req.ChatTemplateKwargs["enable_thinking"])
	}
	if req.MaxTokens != maxResponseTokens {
		t.Fatalf("max_tokens = %d, want %d", req.MaxTokens, maxResponseTokens)
	}
}

func TestChatViewShowsThinkingAndSelectedTerminal(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.activeChat = store.Chat{ID: "chat-1", Title: "Project", Thinking: true}
	m.activeAgent = store.Agent{Name: "Helper"}
	m.terminals = []*terminalEntry{{
		id:   "terminal-1",
		name: "Terminal 1",
		cwd:  "/project",
	}}
	m.activeTerminalID = "terminal-1"
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.viewChat()
	for _, status := range []string{"THINKING ON", "TERMINAL: Terminal 1"} {
		if !strings.Contains(view, status) {
			t.Fatalf("chat view does not show %q: %s", status, view)
		}
	}

	m.activeChat.Thinking = false
	view = m.viewChat()
	if !strings.Contains(view, "THINKING OFF") {
		t.Fatalf("chat view does not show thinking off: %s", view)
	}
}

func TestCreateChatPersistsTitleAndAssignedAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New(api.NewClient("", ""), config.Default())
	m.workspace.Agents = []store.Agent{{ID: "agent-1", Name: "Helper"}}
	m.chatTitleInput.SetValue("Project chat")
	m.newChatAgent = 0

	if err := m.createChat(); err != nil {
		t.Fatalf("createChat: %v", err)
	}
	workspace, err := store.LoadWorkspace()
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if len(workspace.Chats) != 1 {
		t.Fatalf("chats = %+v, want one", workspace.Chats)
	}
	chat := workspace.Chats[0]
	if chat.Title != "Project chat" || chat.AgentID != "agent-1" || !chat.Thinking {
		t.Fatalf("unexpected chat: %+v", chat)
	}
	if m.state != stateChat || m.activeChat.ID != chat.ID {
		t.Fatalf("created chat was not opened: state=%v active=%+v", m.state, m.activeChat)
	}
}

func TestSaveAgentPersistsSystemPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New(api.NewClient("", ""), config.Default())
	m.agentNameInput.SetValue("Reviewer")
	m.agentPrompt.SetValue("Review code carefully.")

	if err := m.saveAgent(); err != nil {
		t.Fatalf("saveAgent: %v", err)
	}
	workspace, err := store.LoadWorkspace()
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if len(workspace.Agents) != 1 || workspace.Agents[0].Name != "Reviewer" || workspace.Agents[0].SystemPrompt != "Review code carefully." {
		t.Fatalf("agent profile did not persist: %+v", workspace.Agents)
	}
}

func TestFitChatRequestDropsOldestWholeTurns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apply-template":
			var body struct {
				Messages []api.Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode template request: %v", err)
			}
			var prompt strings.Builder
			for _, message := range body.Messages {
				prompt.WriteString(message.Content)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"prompt": prompt.String()})
		case "/tokenize":
			var body struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode tokenize request: %v", err)
			}
			tokens := make([]int, len(body.Content))
			_ = json.NewEncoder(w).Encode(map[string][]int{"tokens": tokens})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	req := api.ChatRequest{Messages: []api.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("old user ", 4)},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "call_1"}}},
		{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("tool output ", 4)},
		{Role: "assistant", Content: strings.Repeat("old answer ", 4)},
		{Role: "user", Content: "new"},
	}}
	fitted, trimmed, err := fitChatRequest(context.Background(), api.NewClient(server.URL, ""), req, 10)
	if err != nil {
		t.Fatalf("fitChatRequest: %v", err)
	}
	if trimmed != 4 {
		t.Fatalf("trimmed messages = %d, want 4", trimmed)
	}
	if len(fitted.Messages) != 2 || fitted.Messages[0].Role != "system" || fitted.Messages[1].Content != "new" {
		t.Fatalf("fitted messages = %+v, want system prompt and latest user message", fitted.Messages)
	}
	if len(req.Messages) != 6 {
		t.Fatalf("original request was modified: %+v", req.Messages)
	}
}

func TestMergeToolCallDeltas(t *testing.T) {
	calls := mergeToolCallDeltas(nil, []api.ToolCall{{
		Index: 0,
		ID:    "call_1",
		Type:  "function",
		Function: api.FunctionArg{
			Name:      "run_bash",
			Arguments: `{"command":`,
		},
	}})
	calls = mergeToolCallDeltas(calls, []api.ToolCall{{
		Index: 0,
		Function: api.FunctionArg{
			Arguments: `"pwd"}`,
		},
	}})

	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Type != "function" || calls[0].ID != "call_1" || calls[0].Function.Name != "run_bash" {
		t.Fatalf("merged tool call metadata = %+v", calls[0])
	}
	command, err := parseRunBashCommand(calls[0].Function.Arguments)
	if err != nil {
		t.Fatalf("parseRunBashCommand: %v", err)
	}
	if command != "pwd" {
		t.Fatalf("command = %q, want pwd", command)
	}
}

func TestParseRunBashCommandRejectsInvalidArguments(t *testing.T) {
	if _, err := parseRunBashCommand(`{"command":`); err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if _, err := parseRunBashCommand(`{"command":"  "}`); err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestNormalizeToolCallHistoryRepairsLegacyDeltas(t *testing.T) {
	messages := []api.Message{{
		Role: "assistant",
		ToolCalls: []api.ToolCall{
			{
				Index: 0,
				ID:    "call_1",
				Function: api.FunctionArg{
					Name:      "run_bash",
					Arguments: `{"command":`,
				},
			},
			{
				Index: 0,
				Function: api.FunctionArg{
					Arguments: `"pwd"}`,
				},
			},
		},
	}}

	normalizeToolCallHistory(messages)

	if len(messages[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want one merged call", len(messages[0].ToolCalls))
	}
	call := messages[0].ToolCalls[0]
	if call.Type != "function" {
		t.Fatalf("tool call type = %q, want function", call.Type)
	}
	command, err := parseRunBashCommand(call.Function.Arguments)
	if err != nil {
		t.Fatalf("parse repaired arguments: %v", err)
	}
	if command != "pwd" {
		t.Fatalf("command = %q, want pwd", command)
	}
}
