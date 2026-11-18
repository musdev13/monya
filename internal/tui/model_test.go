package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"monya/internal/api"
	"monya/internal/config"
	"monya/internal/store"
	"monya/internal/terminal"
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
	if err := config.SaveApprovedCommands([]string{"docker ps"}); err != nil {
		t.Fatalf("SaveApprovedCommands: %v", err)
	}

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
	if len(loaded.approvedCommands) != 1 || loaded.approvedCommands[0] != "docker ps" {
		t.Fatalf("approved commands = %v, want docker ps", loaded.approvedCommands)
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

func TestTrustedExecutionTogglePersistsForCurrentChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{{ID: "chat-1", Title: "Test", AgentID: "agent-1"}},
	}
	m.activeChat = m.workspace.Chats[0]
	m.activeAgent = m.workspace.Agents[0]
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyF4})

	if !m.activeChat.TrustExecution {
		t.Fatal("trusted execution should be enabled")
	}
	if !strings.Contains(m.chatStatusLine(), "TRUST EXEC ON") {
		t.Fatal("trusted execution status is not visible")
	}
	workspace, err := store.LoadWorkspace()
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if len(workspace.Chats) != 1 || !workspace.Chats[0].TrustExecution {
		t.Fatalf("trusted execution mode was not persisted: %+v", workspace.Chats)
	}
}

func TestTrustedExecutionAutoRunsOnlySafeCommands(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{command: "pwd", want: true},
		{command: "cd /tmp", want: true},
		{command: "ls src", want: true},
		{command: "cat internal/tui/model.go", want: true},
		{command: "grep package internal/tui/model.go", want: true},
		{command: "sed -n '1,10p' internal/tui/model.go", want: true},
		{command: "sed --sandbox -n 1,10p internal/tui/model.go", want: true},
		{command: "sed -i s/old/new/ file", want: false},
		{command: "sed --sandbox -n e file", want: false},
		{command: "ls -la", want: false},
		{command: "cat", want: false},
		{command: "rm -rf build", want: false},
		{command: "ls; curl example.com", want: false},
		{command: "pwd $(whoami)", want: false},
	} {
		if got := isSafeAutoCommand(test.command); got != test.want {
			t.Errorf("isSafeAutoCommand(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestTrustedExecutionStartsSafeToolWithoutConfirmation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session, err := terminal.Start()
	if err != nil {
		t.Fatalf("terminal.Start: %v", err)
	}
	defer session.Close()

	chat := store.Chat{
		ID:             strings.Repeat("a", 32),
		Title:          "Test",
		AgentID:        "agent-1",
		TrustExecution: true,
	}
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{chat},
	}
	m.activeChat = chat
	m.activeAgent = m.workspace.Agents[0]
	m.terminals = []*terminalEntry{{id: "terminal-1", name: "Terminal 1", session: session}}
	m.activeTerminalID = "terminal-1"
	m.currentToolCalls = []api.ToolCall{{
		ID:       "tool-1",
		Type:     "function",
		Function: api.FunctionArg{Name: "run_bash", Arguments: `{"command":"pwd"}`},
	}}

	_, cmd := m.Update(streamDoneMsg{})

	if m.pendingToolCall != nil || !m.commandRunning {
		t.Fatalf("safe command was not started automatically: pending=%v running=%v", m.pendingToolCall, m.commandRunning)
	}
	if cmd == nil {
		t.Fatal("automatic execution did not start any command")
	}
}

func TestRememberedCommandsMustBeLiteral(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{command: "docker ps", want: true},
		{command: "go test ./...", want: true},
		{command: "docker ps && echo done", want: false},
		{command: "cat $HOME/.ssh/id_rsa", want: false},
		{command: `echo "hello"`, want: false},
	} {
		if got := isRememberableCommand(test.command); got != test.want {
			t.Errorf("isRememberableCommand(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestTrustedExecutionRemembersExactApprovedCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session, err := terminal.Start()
	if err != nil {
		t.Fatalf("terminal.Start: %v", err)
	}
	defer session.Close()

	chat := store.Chat{
		ID:             strings.Repeat("b", 32),
		Title:          "Test",
		AgentID:        "agent-1",
		TrustExecution: true,
	}
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{chat},
	}
	m.activeChat = chat
	m.activeAgent = m.workspace.Agents[0]
	m.terminals = []*terminalEntry{{id: "terminal-1", name: "Terminal 1", session: session}}
	m.activeTerminalID = "terminal-1"
	m.currentToolCalls = []api.ToolCall{{
		ID:       "tool-1",
		Type:     "function",
		Function: api.FunctionArg{Name: "run_bash", Arguments: `{"command":"docker ps"}`},
	}}

	_, _ = m.Update(streamDoneMsg{})
	if m.pendingToolCall == nil || m.commandRunning {
		t.Fatalf("unapproved command should wait for confirmation: pending=%v running=%v", m.pendingToolCall, m.commandRunning)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !m.commandRunning || cmd == nil {
		t.Fatal("approved command did not start")
	}
	if !m.shouldAutoRunCommand("docker ps") {
		t.Fatal("the exact approved command was not remembered")
	}
	if m.shouldAutoRunCommand("docker ps -a") {
		t.Fatal("a different command was implicitly approved")
	}
	m.activeChat.TrustExecution = false
	if m.shouldAutoRunCommand("docker ps") {
		t.Fatal("saved approvals must not bypass confirmation when trusted execution is off")
	}
	saved, err := config.LoadApprovedCommands()
	if err != nil {
		t.Fatalf("LoadApprovedCommands: %v", err)
	}
	if len(saved) != 1 || saved[0] != "docker ps" {
		t.Fatalf("saved approved commands = %v, want exact docker ps command", saved)
	}
}

func TestReadFileToolRunsWithoutCommandApproval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("hello from project\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	chat := store.Chat{
		ID:      strings.Repeat("c", 32),
		Title:   "Test",
		AgentID: "agent-1",
	}
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspaceRoot = project
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{chat},
	}
	m.activeChat = chat
	m.activeAgent = m.workspace.Agents[0]
	m.currentToolCalls = []api.ToolCall{{
		ID:   "file-call",
		Type: "function",
		Function: api.FunctionArg{
			Name:      "read_file",
			Arguments: `{"path":"notes.txt"}`,
		},
	}}

	_, cmd := m.Update(streamDoneMsg{})
	if m.pendingToolCall != nil || m.commandRunning {
		t.Fatal("read_file incorrectly entered the shell approval flow")
	}
	if cmd == nil {
		t.Fatal("read_file did not start")
	}
	msg := cmd()
	result, ok := msg.(fileToolDoneMsg)
	if !ok {
		t.Fatalf("file tool returned %T, want fileToolDoneMsg", msg)
	}
	if result.err != nil || !strings.Contains(result.result, "hello from project") {
		t.Fatalf("file tool result = %+v", result)
	}

	_, _ = m.Update(result)
	if got := m.history[len(m.history)-1].Content; !strings.Contains(got, "hello from project") {
		t.Fatalf("tool result was not added to chat history: %q", got)
	}
}

func TestFileToolsRunSequentiallyWithoutApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "source.txt"), []byte("read first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	chat := store.Chat{
		ID:      strings.Repeat("d", 32),
		Title:   "Test",
		AgentID: "agent-1",
	}
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.workspaceRoot = project
	m.workspace = store.Workspace{
		Agents: []store.Agent{{ID: "agent-1", Name: "Helper"}},
		Chats:  []store.Chat{chat},
	}
	m.activeChat = chat
	m.activeAgent = m.workspace.Agents[0]
	m.currentToolCalls = []api.ToolCall{
		{
			ID:       "read-call",
			Type:     "function",
			Function: api.FunctionArg{Name: "read_file", Arguments: `{"path":"source.txt"}`},
		},
		{
			ID:       "create-call",
			Type:     "function",
			Function: api.FunctionArg{Name: "create_file", Arguments: `{"path":"new.txt","content":"created second"}`},
		},
	}

	_, cmd := m.Update(streamDoneMsg{})
	if m.pendingToolCall != nil || cmd == nil {
		t.Fatal("file tools unexpectedly required approval or did not start")
	}
	first := cmd().(fileToolDoneMsg)
	_, cmd = m.Update(first)
	if cmd == nil {
		t.Fatal("second file tool did not start")
	}
	second := cmd().(fileToolDoneMsg)
	_, _ = m.Update(second)
	if m.pendingToolCall != nil || !m.isStreaming {
		t.Fatalf("file tools did not complete before resuming the model: pending=%v streaming=%v", m.pendingToolCall, m.isStreaming)
	}

	if got, err := os.ReadFile(filepath.Join(project, "new.txt")); err != nil || string(got) != "created second" {
		t.Fatalf("created file = %q, err=%v", got, err)
	}
	var readResult, createResult string
	for _, message := range m.history {
		if message.Role != "tool" {
			continue
		}
		switch message.Name {
		case "read_file":
			readResult = message.Content
		case "create_file":
			createResult = message.Content
		}
	}
	if !strings.Contains(readResult, "1: read first") {
		t.Fatalf("read_file result = %q", readResult)
	}
	if !strings.Contains(createResult, "Created new.txt") {
		t.Fatalf("create_file result = %q", createResult)
	}
}

func TestChatRequestUsesAgentPromptAndSavedThinkingMode(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.cfg.SkillsPrompt = "You are Monya."
	m.activeChat = store.Chat{ID: "chat-1", Thinking: false}
	m.activeAgent = store.Agent{Name: "Reviewer", SystemPrompt: "Review carefully."}
	m.history = []api.Message{{Role: "user", Content: "Check this."}}

	req := m.chatRequest()
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Fatalf("system prompt missing from request: %+v", req.Messages)
	}
	promptParts := strings.SplitN(req.Messages[0].Content, "\n\n", 2)
	const dateTimePrefix = "Current local date and time (RFC3339): "
	if !strings.HasPrefix(promptParts[0], dateTimePrefix) {
		t.Fatalf("current date/time missing from system prompt: %q", promptParts[0])
	}
	dateTime := strings.TrimSuffix(strings.TrimPrefix(promptParts[0], dateTimePrefix),
		". Treat this as authoritative; do not assume a different date or year.")
	if _, err := time.Parse(time.RFC3339, dateTime); err != nil {
		t.Fatalf("date/time %q is not RFC3339: %v", dateTime, err)
	}
	if len(promptParts) != 2 || promptParts[1] != "You are Monya.\n\nReview carefully." {
		t.Fatalf("skills and agent prompts missing from request: %q", req.Messages[0].Content)
	}
	if req.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("thinking mode = %v, want false", req.ChatTemplateKwargs["enable_thinking"])
	}
	if req.MaxTokens != maxResponseTokens {
		t.Fatalf("max_tokens = %d, want %d", req.MaxTokens, maxResponseTokens)
	}
	if len(req.Tools) != 3 || req.Tools[0].Function.Name != "run_bash" ||
		req.Tools[1].Function.Name != "read_file" || req.Tools[2].Function.Name != "create_file" {
		t.Fatalf("tools = %+v, want run_bash, read_file, create_file", req.Tools)
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
	for _, status := range []string{"THINKING ON", "TRUST EXEC OFF", "TERMINAL: Terminal 1"} {
		if !strings.Contains(view, status) {
			t.Fatalf("chat view does not show %q: %s", status, view)
		}
	}
	m.isStreaming = true
	view = m.viewChat()
	for _, status := range []string{"THINKING ON", "TRUST EXEC OFF", "TERMINAL: Terminal 1"} {
		if !strings.Contains(view, status) {
			t.Fatalf("streaming chat view does not show %q: %s", status, view)
		}
	}

	m.isStreaming = false
	m.activeChat.Thinking = false
	view = m.viewChat()
	if !strings.Contains(view, "THINKING OFF") {
		t.Fatalf("chat view does not show thinking off: %s", view)
	}
}

func TestChatStatusLineWrapsWithinNarrowWindow(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.activeChat.Thinking = true
	m.terminals = []*terminalEntry{{id: "terminal-1", name: "Terminal 1"}}
	m.activeTerminalID = "terminal-1"
	_, _ = m.Update(tea.WindowSizeMsg{Width: 15, Height: 20})

	for _, line := range strings.Split(m.chatStatusLine(), "\n") {
		if width := lipgloss.Width(line); width > m.viewport.Width {
			t.Fatalf("status line width %d exceeds viewport width %d: %q", width, m.viewport.Width, line)
		}
	}
}

func TestCodeBlockClickUsesViewportScreenCoordinates(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.viewport.Width = 80
	m.viewport.Height = 30
	m.history = []api.Message{{
		Role:    "assistant",
		Content: "```go\nfmt.Println(\"hello\")\n```",
	}}
	m.updateViewport()

	if len(m.codeHits) != 1 {
		t.Fatalf("code hits = %d, want one", len(m.codeHits))
	}
	code, ok := m.codeBlockAtScreenY(2 + m.codeHits[0].startLine)
	if !ok {
		t.Fatal("clicking the code block's top border did not hit it")
	}
	if code != "fmt.Println(\"hello\")" {
		t.Fatalf("clicked code = %q", code)
	}
}

func TestTerminalCanBeSelectedWithoutOpeningConsole(t *testing.T) {
	session, err := terminal.Start()
	if err != nil {
		t.Fatalf("terminal.Start: %v", err)
	}
	defer session.Close()

	m := New(api.NewClient("", ""), config.Default())
	m.state = stateTerminals
	m.terminalReturnState = stateChat
	m.list.Title = "Monya — Terminals"
	m.list.SetSize(80, 20)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.terminals = []*terminalEntry{
		{id: "terminal-1", name: "Terminal 1", session: session},
		{id: "terminal-2", name: "Terminal 2", session: session},
	}
	m.activeTerminalID = "terminal-1"
	m.list.SetItems(m.terminalListItems())
	_, _ = m.updateTerminalList(tea.KeyMsg{Type: tea.KeyDown})

	_, _ = m.updateTerminalList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if m.activeTerminalID != "terminal-2" {
		t.Fatalf("active terminal = %q, want terminal-2", m.activeTerminalID)
	}
	if m.state != stateChat {
		t.Fatalf("state = %v, want chat (selection should not open console)", m.state)
	}
	if status := m.chatStatusLine(); !strings.Contains(status, "TERMINAL: Terminal 2") {
		t.Fatalf("selected terminal is not shown in chat status: %s", status)
	}
}

func TestChatTextWrapsToViewportWidth(t *testing.T) {
	m := New(api.NewClient("", ""), config.Default())
	m.state = stateChat
	m.history = []api.Message{{
		Role:             "assistant",
		ReasoningContent: strings.Repeat("reasoning words ", 8),
		Content:          strings.Repeat("answer words ", 8),
	}}
	_, _ = m.Update(tea.WindowSizeMsg{Width: 24, Height: 20})
	m.updateViewport()

	for _, line := range strings.Split(m.viewport.View(), "\n") {
		if width := lipgloss.Width(line); width > m.viewport.Width {
			t.Fatalf("rendered line width %d exceeds viewport width %d: %q", width, m.viewport.Width, line)
		}
	}
}

func TestLongMarkdownAndCodeLinesWrapWithoutLosingText(t *testing.T) {
	long := strings.Repeat("unbroken", 8)
	rendered := renderMarkdown(long, 12)
	for _, line := range strings.Split(strings.TrimSpace(rendered), "\n") {
		if width := lipgloss.Width(line); width > 12 {
			t.Fatalf("markdown line width %d exceeds 12: %q", width, line)
		}
	}

	wrapped := wrapCodeLine(long, 7)
	if strings.Join(wrapped, "") != long {
		t.Fatalf("wrapped code changed content: %q", strings.Join(wrapped, ""))
	}
	for _, line := range wrapped {
		if width := lipgloss.Width(line); width > 7 {
			t.Fatalf("code line width %d exceeds 7: %q", width, line)
		}
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
