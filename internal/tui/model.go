package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"monya/internal/agenttools"
	"monya/internal/api"
	"monya/internal/config"
	"monya/internal/store"
	"monya/internal/terminal"
)

type state int

const (
	stateChats state = iota
	stateChat
	stateNewChat
	stateAgents
	stateEditAgent
	stateConfirmDeleteAgent
	stateConfirmDeleteChat
	stateTerminals
	stateTerminal
	stateConfirmCloseTerminal
)

const (
	maxResponseTokens   = 1024
	maxResponseTokenCap = 4096
	contextSafetyTokens = 64
)

var safeSedScript = regexp.MustCompile(`^([0-9]+(,[0-9]+)?)?(p|d|q|=|s/[^/;]+/[^/;]*/g?)$`)

type (
	workspaceLoadedMsg struct {
		workspace        store.Workspace
		approvedCommands []string
		err              error
	}

	streamStartedMsg struct {
		stream         *api.Stream
		contextTrimmed int
		maxTokens      int
	}
	chunkMsg      api.StreamChunk
	streamDoneMsg struct{}
	streamErrMsg  struct{ err error }

	terminalStartedMsg struct {
		id      string
		session *terminal.Session
		err     error
	}

	terminalCommandDoneMsg struct {
		terminalID string
		result     terminal.Result
		aiTool     bool
		err        error
	}
	terminalInputDoneMsg struct {
		terminalID string
		input      string
		err        error
	}
	fileToolDoneMsg struct {
		callID string
		name   string
		result string
		err    error
	}
	terminalOutputTickMsg struct{ terminalID string }
	commandTimeoutMsg     struct{ token int }
	clearCopyStatusMsg    struct{ token int }
)

type chatItem struct {
	chat      store.Chat
	agentName string
}

func (i chatItem) Title() string { return i.chat.Title }
func (i chatItem) Description() string {
	toolsLabel := "off"
	if i.chat.ToolsEnabled {
		toolsLabel = "on"
	}
	return fmt.Sprintf("%s · thinking %s · tools %s", i.agentName, thinkingLabel(i.chat.Thinking), toolsLabel)
}
func (i chatItem) FilterValue() string { return i.chat.Title + " " + i.agentName }

type agentItem struct{ agent store.Agent }

func (i agentItem) Title() string { return i.agent.Name }
func (i agentItem) Description() string {
	if i.agent.SystemPrompt == "" {
		return "No system prompt"
	}
	prompt := strings.TrimSpace(i.agent.SystemPrompt)
	runes := []rune(prompt)
	if len(runes) > 80 {
		prompt = string(runes[:77]) + "..."
	}
	return prompt
}
func (i agentItem) FilterValue() string { return i.agent.Name + " " + i.agent.SystemPrompt }

type terminalEntry struct {
	id       string
	name     string
	cwd      string
	session  *terminal.Session
	output   string
	starting bool
	err      string
}

type terminalItem struct {
	terminal *terminalEntry
	active   bool
}

func (i terminalItem) Title() string { return i.terminal.name }
func (i terminalItem) Description() string {
	state := i.terminal.cwd
	if i.terminal.starting {
		state = "starting..."
	} else if i.terminal.err != "" {
		state = "unavailable: " + i.terminal.err
	}
	if i.active {
		return "ACTIVE · " + state
	}
	return state
}
func (i terminalItem) FilterValue() string { return i.terminal.name + " " + i.terminal.cwd }

type Model struct {
	client           *api.Client
	cfg              *config.Config
	workspaceRoot    string
	workspaceRootErr error

	state state
	err   error
	busy  bool

	workspace store.Workspace
	list      list.Model

	chatTitleInput     textinput.Model
	newChatAgent       int
	agentNameInput     textinput.Model
	agentPrompt        textarea.Model
	editingAgentID     string
	agentError         string
	deleteAgentID      string
	deleteChatID       string
	deleteChatName     string
	deleteTerminalID   string
	deleteTerminalName string

	terminals            []*terminalEntry
	activeTerminalID     string
	nextTerminalID       int
	terminalReturnState  state
	commandRunning       bool
	commandTerminalID    string
	terminalInputPending bool
	commandTimeoutID     int

	// Chat
	activeChat  store.Chat
	activeAgent store.Agent
	viewport    viewport.Model
	textarea    textarea.Model
	history     []api.Message

	// Streaming state
	spinner             spinner.Model
	isStreaming         bool
	stream              *api.Stream
	cancelStream        context.CancelFunc
	currentReasoning    string
	currentResponse     string
	currentToolCalls    []api.ToolCall
	toolCallQueue       []api.ToolCall
	currentFinishReason string
	finalAnswerRetry    bool
	finalAnswerTokens   int
	currentMaxTokens    int
	fileToolRunning     bool

	// Command execution state
	pendingToolCall  *api.ToolCall
	pendingCmd       string
	commandTimedOut  bool
	approvedCommands map[string]struct{}

	// Clipboard feedback
	copyStatus      string
	copyStatusToken int
	contextNotice   string
	terminalNotice  string
	chatError       string
	storageError    string

	// Mouse capture: true = wheel scrolls, false = native text selection
	mouseOn bool

	// Clickable code blocks inside the viewport content
	codeHits []codeBlockHit
}

func New(client *api.Client, cfg *config.Config) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message..."
	ta.Focus()
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")
	ta.CharLimit = 0

	vp := viewport.New(0, 0)

	delegate := list.NewDefaultDelegate()
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Monya — Chats"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	l.AdditionalFullHelpKeys = func() []key.Binding {
		if l.Title == "Monya — Agents" {
			return []key.Binding{
				key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new agent")),
				key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete agent")),
			}
		}
		if l.Title == "Monya — Terminals" {
			return []key.Binding{
				key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new terminal")),
				key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "select for Monya")),
				key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "close terminal")),
				key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart unavailable")),
			}
		}
		return []key.Binding{
			key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new chat")),
			key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "agents")),
			key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete chat")),
		}
	}

	ti := textinput.New()
	ti.Placeholder = "Chat title..."
	ti.CharLimit = 64

	agentName := textinput.New()
	agentName.Placeholder = "Agent name..."
	agentName.CharLimit = 64

	agentPrompt := textarea.New()
	agentPrompt.Placeholder = "System prompt..."
	agentPrompt.CharLimit = 0
	agentPrompt.KeyMap.InsertNewline.SetKeys("ctrl+j")

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	workspaceRoot, workspaceRootErr := os.Getwd()
	m := &Model{
		client:           client,
		cfg:              cfg,
		workspaceRoot:    workspaceRoot,
		workspaceRootErr: workspaceRootErr,
		state:            stateChats,
		busy:             true,
		mouseOn:          true,
		textarea:         ta,
		viewport:         vp,
		list:             l,
		chatTitleInput:   ti,
		agentNameInput:   agentName,
		agentPrompt:      agentPrompt,
		spinner:          sp,
		approvedCommands: make(map[string]struct{}),
	}
	m.terminals = []*terminalEntry{{id: "terminal-1", name: "Terminal 1", starting: true}}
	m.activeTerminalID = "terminal-1"
	m.nextTerminalID = 2
	return m
}

func loadWorkspace(defaultAgent string) tea.Cmd {
	return func() tea.Msg {
		approvedCommands, err := config.LoadApprovedCommands()
		if err != nil {
			return workspaceLoadedMsg{err: fmt.Errorf("load approved commands: %w", err)}
		}
		workspace, err := store.LoadWorkspace()
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}
		changed, err := store.MigrateLegacyHistories(&workspace)
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}
		if len(workspace.Agents) == 0 {
			id, idErr := store.NewID()
			if idErr != nil {
				return workspaceLoadedMsg{err: idErr}
			}
			workspace.Agents = append(workspace.Agents, store.Agent{ID: id, Name: defaultAgent})
			changed = true
		}
		defaultAgentID := workspace.Agents[0].ID
		for _, agent := range workspace.Agents {
			if agent.Name == defaultAgent {
				defaultAgentID = agent.ID
				break
			}
		}
		for i := range workspace.Chats {
			if !hasAgent(workspace.Agents, workspace.Chats[i].AgentID) {
				workspace.Chats[i].AgentID = defaultAgentID
				changed = true
			}
		}
		if len(workspace.Chats) == 0 {
			id, idErr := store.NewID()
			if idErr != nil {
				return workspaceLoadedMsg{err: idErr}
			}
			workspace.Chats = append(workspace.Chats, store.Chat{
				ID:        id,
				Title:     "New chat",
				AgentID:   defaultAgentID,
				Thinking:  true,
				UpdatedAt: time.Now(),
			})
			changed = true
		}
		if changed {
			if err := store.SaveWorkspace(workspace); err != nil {
				return workspaceLoadedMsg{err: err}
			}
		}
		return workspaceLoadedMsg{workspace: workspace, approvedCommands: approvedCommands}
	}
}

func hasAgent(agents []store.Agent, id string) bool {
	for _, agent := range agents {
		if agent.ID == id {
			return true
		}
	}
	return false
}

func findAgent(agents []store.Agent, id string) (store.Agent, bool) {
	for _, agent := range agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return store.Agent{}, false
}

func thinkingLabel(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (m *Model) startTerminalSession(id string) tea.Cmd {
	return func() tea.Msg {
		session, err := terminal.Start()
		return terminalStartedMsg{id: id, session: session, err: err}
	}
}

func executeTerminalCommand(id string, session *terminal.Session, command string, aiTool bool) tea.Cmd {
	return func() tea.Msg {
		result, err := session.Run(command)
		return terminalCommandDoneMsg{terminalID: id, result: result, aiTool: aiTool, err: err}
	}
}

func sendTerminalInput(id string, session *terminal.Session, input string) tea.Cmd {
	return func() tea.Msg {
		err := session.SendInput(input + "\n")
		return terminalInputDoneMsg{terminalID: id, input: input, err: err}
	}
}

func terminalOutputTick(id string) tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return terminalOutputTickMsg{terminalID: id}
	})
}

func (m *Model) terminalByID(id string) *terminalEntry {
	for _, entry := range m.terminals {
		if entry.id == id {
			return entry
		}
	}
	return nil
}

func (m *Model) activeTerminal() *terminalEntry {
	return m.terminalByID(m.activeTerminalID)
}

func (m *Model) closeTerminalSessions() {
	for _, entry := range m.terminals {
		if entry.session == nil {
			continue
		}
		if err := entry.session.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "monya: close %s: %v\n", entry.name, err)
		}
		entry.session = nil
	}
}

func (m *Model) terminalListItems() []list.Item {
	items := make([]list.Item, 0, len(m.terminals))
	for _, entry := range m.terminals {
		items = append(items, terminalItem{terminal: entry, active: entry.id == m.activeTerminalID})
	}
	return items
}

func (m *Model) appendTerminalOutput(entry *terminalEntry, text string) {
	const maxTerminalLog = 64 * 1024
	if text == "" {
		return
	}
	entry.output += text
	if !strings.HasSuffix(entry.output, "\n") {
		entry.output += "\n"
	}
	if len(entry.output) > maxTerminalLog {
		entry.output = "[earlier terminal output omitted]\n" + entry.output[len(entry.output)-maxTerminalLog:]
	}
}

func (m *Model) updateTerminalViewport(entry *terminalEntry) {
	m.viewport.SetContent(entry.output)
	m.viewport.GotoBottom()
}

func (m *Model) agentName(id string) string {
	agent, ok := findAgent(m.workspace.Agents, id)
	if !ok {
		return "Unknown agent"
	}
	return agent.Name
}

func (m *Model) chatListItems() []list.Item {
	chats := append([]store.Chat(nil), m.workspace.Chats...)
	sort.SliceStable(chats, func(i, j int) bool {
		return chats[i].UpdatedAt.After(chats[j].UpdatedAt)
	})
	items := make([]list.Item, 0, len(chats))
	for _, chat := range chats {
		items = append(items, chatItem{chat: chat, agentName: m.agentName(chat.AgentID)})
	}
	return items
}

func (m *Model) agentListItems() []list.Item {
	items := make([]list.Item, 0, len(m.workspace.Agents))
	for _, agent := range m.workspace.Agents {
		items = append(items, agentItem{agent: agent})
	}
	return items
}

func (m *Model) openChat(chat store.Chat) error {
	history, err := store.LoadChatHistory(chat.ID)
	if err != nil {
		return err
	}
	agent, ok := findAgent(m.workspace.Agents, chat.AgentID)
	if !ok {
		return fmt.Errorf("agent for chat %q not found", chat.Title)
	}
	m.activeChat = chat
	m.activeAgent = agent
	m.history = history
	if m.history == nil {
		m.history = []api.Message{}
	}
	m.state = stateChat
	m.isStreaming = false
	m.pendingToolCall = nil
	m.chatError = ""
	m.updateViewport()
	m.textarea.SetValue("")
	m.textarea.Placeholder = "Type a message..."
	m.textarea.Focus()
	return nil
}

func (m *Model) persistActiveChat() {
	if err := store.SaveChatHistory(m.activeChat.ID, m.history); err != nil {
		m.storageError = fmt.Sprintf("Save chat: %v", err)
		return
	}
	m.storageError = ""
	m.activeChat.UpdatedAt = time.Now()
	for i := range m.workspace.Chats {
		if m.workspace.Chats[i].ID == m.activeChat.ID {
			m.workspace.Chats[i] = m.activeChat
			break
		}
	}
	if err := store.SaveWorkspace(m.workspace); err != nil {
		m.storageError = fmt.Sprintf("Save chat list: %v", err)
	}
}

func (m *Model) readNextChunk() tea.Cmd {
	return func() tea.Msg {
		if m.stream == nil {
			return streamDoneMsg{}
		}
		chunk, err := m.stream.Next()
		if errors.Is(err, io.EOF) {
			return streamDoneMsg{}
		}
		if err != nil {
			return streamErrMsg{err: err}
		}
		return chunkMsg(*chunk)
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		loadWorkspace(m.cfg.DefaultAgent),
		m.startTerminalSession(m.terminals[0].id),
		m.list.StartSpinner(),
		m.spinner.Tick,
	)
}

func (m *Model) chatRequest() api.ChatRequest {
	reqHistory := make([]api.Message, len(m.history))
	copy(reqHistory, m.history)
	for i := range reqHistory {
		reqHistory[i].ReasoningContent = "" // API не ожидает это поле в запросе
	}
	normalizeToolCallHistory(reqHistory)
	if !m.activeChat.ToolsEnabled {
		reqHistory = stripToolHistory(reqHistory)
	}
	prompts := []string{m.cfg.SkillsPrompt}
	if m.activeChat.ToolsEnabled {
		prompts = append(prompts, m.cfg.SkillsInstrumentsPrompt)
	}
	prompts = append(prompts, m.activeAgent.SystemPrompt)
	var systemPrompt strings.Builder
	for _, prompt := range prompts {
		if prompt = strings.TrimSpace(prompt); prompt != "" {
			if systemPrompt.Len() > 0 {
				systemPrompt.WriteString("\n\n")
			}
			systemPrompt.WriteString(prompt)
		}
	}
	if systemPrompt.Len() > 0 {
		systemPrompt.WriteString("\n\n")
	}
	systemPrompt.WriteString("Current local date and time (RFC3339): ")
	systemPrompt.WriteString(time.Now().Format(time.RFC3339))
	reqHistory = append([]api.Message{{Role: "system", Content: systemPrompt.String()}}, reqHistory...)

	var tools []api.Tool
	if m.activeChat.ToolsEnabled {
		tools = append(tools, api.Tool{
			Type: "function",
			Function: api.FunctionDef{
				Name:        "run_bash",
				Description: "Выполнить bash-команду в выбранном пользователем терминале Monya и вернуть stdout+stderr. Состояние shell, включая cwd и переменные, сохраняется между вызовами.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"command": {"type": "string", "description": "Bash-команда для выполнения"}
					},
					"required": ["command"]
				}`),
			},
		})
		tools = append(tools, agenttools.Definitions()...)
	}

	maxTokens := maxResponseTokens
	if m.finalAnswerRetry && m.finalAnswerTokens > maxTokens {
		maxTokens = m.finalAnswerTokens
	}
	return api.ChatRequest{
		Model:     m.cfg.DefaultModel,
		MaxTokens: maxTokens,
		User:      m.activeChat.ID,
		Messages:  reqHistory,
		ChatTemplateKwargs: map[string]any{
			"enable_thinking": m.activeChat.Thinking,
		},
		Tools: tools,
	}
}

func (m *Model) startStream() tea.Cmd {
	m.isStreaming = true
	m.currentReasoning = ""
	m.currentResponse = ""
	m.currentToolCalls = nil
	m.currentFinishReason = ""
	m.chatError = ""
	m.contextNotice = ""
	m.updateViewport()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelStream = cancel
	req := m.chatRequest()
	finalAnswerRetry := m.finalAnswerRetry
	finalAnswerTokens := m.finalAnswerTokens

	return func() tea.Msg {
		prepCtx, prepCancel := context.WithTimeout(ctx, 30*time.Second)
		defer prepCancel()

		contextSize, err := m.client.ContextSize(prepCtx)
		if err != nil {
			cancel()
			return streamErrMsg{err: fmt.Errorf("read model context size: %w", err)}
		}
		req.MaxTokens = responseTokenBudget(contextSize)
		if finalAnswerRetry && finalAnswerTokens > req.MaxTokens {
			req.MaxTokens = min(finalAnswerTokens, contextSize/2)
		}
		promptBudget := contextSize - req.MaxTokens - contextSafetyTokens
		fittedReq, trimmed, err := fitChatRequest(prepCtx, m.client, req, promptBudget)
		if err != nil {
			cancel()
			return streamErrMsg{err: err}
		}

		stream, err := m.client.ChatCompletionStream(ctx, fittedReq)
		if err != nil {
			cancel()
			return streamErrMsg{err: err}
		}
		return streamStartedMsg{
			stream:         stream,
			contextTrimmed: trimmed,
			maxTokens:      req.MaxTokens,
		}
	}
}

func responseTokenBudget(contextSize int) int {
	// Reserve a quarter of the context for generation, with bounded minimum and maximum budgets.
	tokens := contextSize / 4
	if tokens < maxResponseTokens {
		return maxResponseTokens
	}
	return min(tokens, maxResponseTokenCap)
}

func fitChatRequest(ctx context.Context, client *api.Client, req api.ChatRequest, promptBudget int) (api.ChatRequest, int, error) {
	if promptBudget <= 0 {
		return api.ChatRequest{}, 0, fmt.Errorf("model context is too small for a %d-token response", req.MaxTokens)
	}

	tokenCount, err := client.CountChatTokens(ctx, req)
	if err != nil {
		return api.ChatRequest{}, 0, err
	}
	if tokenCount <= promptBudget {
		return req, 0, nil
	}

	prefix := 0
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		prefix = 1
	}
	var starts []int
	for i := prefix; i < len(req.Messages); i++ {
		message := req.Messages[i]
		if message.Role == "user" {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return api.ChatRequest{}, 0, fmt.Errorf("cannot compact conversation: no user message found")
	}

	low, high := 0, len(starts)-1
	best := -1
	for low <= high {
		mid := low + (high-low)/2
		candidate := req
		candidate.Messages = append(
			append([]api.Message(nil), req.Messages[:prefix]...),
			req.Messages[starts[mid]:]...,
		)
		count, countErr := client.CountChatTokens(ctx, candidate)
		if countErr != nil {
			return api.ChatRequest{}, 0, countErr
		}
		if count <= promptBudget {
			best = mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	if best == -1 {
		return api.ChatRequest{}, 0, fmt.Errorf("latest user message exceeds available prompt context (%d-token budget)", promptBudget)
	}

	trimmed := starts[best] - prefix
	req.Messages = append(
		append([]api.Message(nil), req.Messages[:prefix]...),
		req.Messages[starts[best]:]...,
	)
	return req, trimmed, nil
}

func mergeToolCallDeltas(calls []api.ToolCall, deltas []api.ToolCall) []api.ToolCall {
	for _, delta := range deltas {
		index := -1
		for i := range calls {
			if calls[i].Index == delta.Index {
				index = i
				break
			}
		}
		if index == -1 {
			calls = append(calls, delta)
			continue
		}

		call := &calls[index]
		if delta.ID != "" {
			call.ID = delta.ID
		}
		if delta.Type != "" {
			call.Type = delta.Type
		}
		if delta.Function.Name != "" {
			call.Function.Name = delta.Function.Name
		}
		call.Function.Arguments += delta.Function.Arguments
	}
	return calls
}

func normalizeToolCallHistory(messages []api.Message) {
	for i := range messages {
		messages[i].ToolCalls = mergeToolCallDeltas(nil, messages[i].ToolCalls)
		for j := range messages[i].ToolCalls {
			if messages[i].ToolCalls[j].Type == "" {
				messages[i].ToolCalls[j].Type = "function"
			}
		}
	}
}

func stripToolHistory(messages []api.Message) []api.Message {
	filtered := make([]api.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "tool" || message.ToolCallID != "" {
			continue
		}
		if len(message.ToolCalls) > 0 {
			message.ToolCalls = nil
			if strings.TrimSpace(message.Content) == "" && message.ReasoningContent == "" {
				continue
			}
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func parseRunBashCommand(arguments string) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("tool arguments do not contain a command")
	}
	return args.Command, nil
}

func isSafeAutoCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	fields := strings.Fields(command)
	if fields[0] == "sed" {
		return isSafeSedCommand(fields)
	}
	if strings.ContainsAny(command, ";&|><`$\\\"'()\n\r*?[]{}!") {
		return false
	}
	switch fields[0] {
	case "pwd", "whoami", "date", "uname":
		return len(fields) == 1
	case "cd":
		return len(fields) <= 2 && (len(fields) == 1 || !strings.HasPrefix(fields[1], "-"))
	case "ls":
		for _, arg := range fields[1:] {
			if strings.HasPrefix(arg, "-") {
				return false
			}
		}
		return true
	case "cat", "head", "tail", "wc", "grep", "rg", "stat", "file":
		if len(fields) < 2 {
			return false
		}
		for _, arg := range fields[1:] {
			if arg == "-" || strings.HasPrefix(arg, "-") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isSafeSedCommand(fields []string) bool {
	if strings.ContainsAny(strings.Join(fields, " "), ";&|><`$\\()\n\r*?[]{}!") {
		return false
	}
	args := fields[1:]
	index := 0
	for index < len(args) {
		switch args[index] {
		case "--sandbox", "-n", "--quiet", "--silent", "-E", "-r", "--regexp-extended":
			index++
			continue
		default:
			if strings.HasPrefix(args[index], "-") {
				return false
			}
		}
		break
	}

	if index >= len(args)-1 {
		return false
	}
	command := args[index]
	if len(command) >= 2 && ((command[0] == '\'' && command[len(command)-1] == '\'') ||
		(command[0] == '"' && command[len(command)-1] == '"')) {
		command = command[1 : len(command)-1]
	}
	if strings.ContainsAny(command, "'\"") || !safeSedScript.MatchString(command) {
		return false
	}
	for _, path := range args[index+1:] {
		if strings.HasPrefix(path, "-") || strings.ContainsAny(path, "'\"") {
			return false
		}
	}
	return true
}

func (m *Model) shouldAutoRunCommand(command string) bool {
	if !m.activeChat.TrustExecution {
		return false
	}
	command = strings.TrimSpace(command)
	if isSafeAutoCommand(command) {
		return true
	}
	_, ok := m.approvedCommands[command]
	return ok
}

func isRememberableCommand(command string) bool {
	command = strings.TrimSpace(command)
	return command != "" && !strings.ContainsAny(command, ";&|><`$\\\"'()\n\r*?[]{}!")
}

func (m *Model) rememberApprovedCommand(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("cannot remember an empty command")
	}
	if !isRememberableCommand(command) {
		return fmt.Errorf("commands with shell operators, quotes, or expansions are not remembered")
	}
	if m.approvedCommands == nil {
		m.approvedCommands = make(map[string]struct{})
	}
	if _, exists := m.approvedCommands[command]; exists {
		return nil
	}
	commands := make([]string, 0, len(m.approvedCommands)+1)
	for approved := range m.approvedCommands {
		commands = append(commands, approved)
	}
	commands = append(commands, command)
	if err := config.SaveApprovedCommands(commands); err != nil {
		return err
	}
	m.approvedCommands[command] = struct{}{}
	return nil
}

func renderMarkdown(content string, width int) string {
	width = max(1, width)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStylePath("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return wrapText(content, width)
	}
	rendered, err := renderer.Render(content)
	if err != nil {
		return wrapText(content, width)
	}
	return ansi.Wrap(rendered, width, "")
}

func wrapText(content string, width int) string {
	width = max(1, width)
	return ansi.Wrap(content, width, "")
}

// extractCodeBlocks returns the contents of all fenced code blocks in md.
func extractCodeBlocks(md string) []string {
	var blocks []string
	var cur []string
	inBlock := false
	for _, ln := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			if inBlock {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
				inBlock = false
			} else {
				inBlock = true
				cur = nil
			}
			continue
		}
		if inBlock {
			cur = append(cur, ln)
		}
	}
	return blocks
}

// codeBlockHit is a clickable region of the viewport content that maps to
// one fenced code block of an assistant message.
type codeBlockHit struct {
	startLine int
	endLine   int
	code      string
}

// mdSegment is a piece of markdown: either plain text or a fenced code block.
type mdSegment struct {
	code bool
	lang string
	text string
}

// splitMarkdown splits md into text and fenced code segments.
func splitMarkdown(md string) []mdSegment {
	var segs []mdSegment
	var buf []string
	inCode := false
	lang := ""
	flushText := func() {
		if len(buf) == 0 {
			return
		}
		segs = append(segs, mdSegment{text: strings.Join(buf, "\n")})
		buf = nil
	}
	for _, ln := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			if inCode {
				segs = append(segs, mdSegment{code: true, lang: lang, text: strings.Join(buf, "\n")})
				buf = nil
				inCode = false
				lang = ""
			} else {
				flushText()
				inCode = true
				lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			}
			continue
		}
		buf = append(buf, ln)
	}
	if len(buf) > 0 {
		segs = append(segs, mdSegment{code: inCode, lang: lang, text: strings.Join(buf, "\n")})
	}
	return segs
}

// renderCodeBox draws a fenced code block inside a rounded frame with a
// [copy] label in the top border. The whole frame is clickable.
func renderCodeBox(code, lang string, width int) string {
	width = max(1, width)
	if width < 5 {
		return wrapText(code, width) + "\n"
	}
	inner := width - 4
	label := "[copy]"
	if lang != "" {
		label = "[" + lang + "] copy"
	}
	if utf8.RuneCountInString(label) > width-3 {
		label = ""
	}
	fill := width - 3 - utf8.RuneCountInString(label)
	var b strings.Builder
	b.WriteString("╭" + "─" + label + strings.Repeat("─", fill) + "╮" + "\n")
	for _, ln := range strings.Split(code, "\n") {
		for _, wrapped := range wrapCodeLine(ln, inner) {
			padding := inner - lipgloss.Width(wrapped)
			if padding < 0 {
				padding = 0
			}
			b.WriteString("│" + " " + wrapped + strings.Repeat(" ", padding) + " " + "│" + "\n")
		}
	}
	b.WriteString("╰" + strings.Repeat("─", inner+2) + "╯" + "\n")
	return b.String()
}

func wrapCodeLine(line string, width int) []string {
	width = max(1, width)
	line = strings.ReplaceAll(line, "\t", "    ")
	return strings.Split(ansi.Hardwrap(line, width, true), "\n")
}

// renderAssistant renders an assistant message, drawing each fenced code
// block as a clickable framed box.
func (m Model) renderAssistant(content string) (string, []codeBlockHit) {
	width := max(1, m.viewport.Width)
	var b strings.Builder
	line := 0
	var hits []codeBlockHit
	for _, seg := range splitMarkdown(content) {
		if seg.code {
			box := renderCodeBox(seg.text, seg.lang, width)
			n := strings.Count(box, "\n")
			hits = append(hits, codeBlockHit{startLine: line, endLine: line + n - 1, code: seg.text})
			b.WriteString(box)
			line += n
			continue
		}
		t := strings.Trim(seg.text, "\n")
		if t == "" {
			continue
		}
		r := renderMarkdown(t, width)
		if !strings.HasSuffix(r, "\n") {
			r += "\n"
		}
		b.WriteString(r)
		line += strings.Count(r, "\n")
	}
	return b.String(), hits
}

// copyToClipboard writes text to the system clipboard. It emits an OSC 52
// escape sequence (supported by many modern terminals) and also tries
// wl-copy/xclip/xsel/pbcopy when available.
func copyToClipboard(text string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\x07", enc)

	for _, c := range [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"pbcopy"},
	} {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return nil
}

// setCopyStatus shows a transient status line in the chat view.
func (m *Model) setCopyStatus(s string) tea.Cmd {
	m.copyStatus = s
	m.copyStatusToken++
	tok := m.copyStatusToken
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return clearCopyStatusMsg{token: tok}
	})
}

// copyText copies s to the clipboard and reports okMsg on success.
func (m *Model) copyText(s, okMsg string) tea.Cmd {
	if err := copyToClipboard(s); err != nil {
		return m.setCopyStatus("Copy failed: " + err.Error())
	}
	return m.setCopyStatus(okMsg)
}

// copyLastAnswer copies the most recent assistant message to the clipboard.
func (m *Model) copyLastAnswer() tea.Cmd {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role == "assistant" && m.history[i].Content != "" {
			if err := copyToClipboard(m.history[i].Content); err != nil {
				return m.setCopyStatus("Copy failed: " + err.Error())
			}
			return m.setCopyStatus("Last answer copied to clipboard")
		}
	}
	return m.setCopyStatus("Nothing to copy")
}

// copyLastCode copies the last fenced code block of the newest assistant
// message that contains one.
func (m *Model) copyLastCode() tea.Cmd {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role != "assistant" {
			continue
		}
		blocks := extractCodeBlocks(m.history[i].Content)
		if len(blocks) == 0 {
			continue
		}
		if err := copyToClipboard(blocks[len(blocks)-1]); err != nil {
			return m.setCopyStatus("Copy failed: " + err.Error())
		}
		return m.setCopyStatus("Last code block copied to clipboard")
	}
	return m.setCopyStatus("No code blocks to copy")
}

func (m *Model) updateViewport() {
	var b strings.Builder
	line := 0
	wrapWidth := max(1, m.viewport.Width)
	m.codeHits = nil
	add := func(s string) {
		b.WriteString(s)
		line += strings.Count(s, "\n")
	}
	addAssistant := func(content string) {
		s, hits := m.renderAssistant(content)
		for i := range hits {
			hits[i].startLine += line
			hits[i].endLine += line
		}
		m.codeHits = append(m.codeHits, hits...)
		add(s)
	}

	for _, msg := range m.history {
		switch msg.Role {
		case "user":
			add(userStyle.Render(wrapText("User: "+msg.Content, wrapWidth)) + "\n")
		case "assistant":
			if msg.ReasoningContent != "" {
				add(reasoningStyle.Render("Thoughts:\n"+wrapText(msg.ReasoningContent, wrapWidth)) + "\n\n")
			}
			add("Assistant:\n")
			if msg.Content != "" {
				addAssistant(msg.Content)
			}
			add("\n")
		case "tool":
			add(toolStyle.Render(wrapText(fmt.Sprintf("Tool (%s): %s", msg.Name, msg.Content), wrapWidth)) + "\n")
		}
		add("\n")
	}

	if m.isStreaming {
		if m.currentReasoning != "" {
			add(dimStyle.Render(wrapText(m.currentReasoning, wrapWidth)) + "\n\n")
		}
		if m.currentResponse != "" {
			add("Assistant:\n")
			addAssistant(m.currentResponse)
		}
	}

	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

func (m *Model) codeBlockAtScreenY(y int) (string, bool) {
	const viewportTop = 2
	if y < viewportTop || y >= viewportTop+m.viewport.Height {
		return "", false
	}
	line := y - viewportTop + m.viewport.YOffset
	for _, hit := range m.codeHits {
		if line >= hit.startLine && line <= hit.endLine {
			return hit.code, true
		}
	}
	return "", false
}

func (m *Model) toggleTrustExecution() {
	oldValue := m.activeChat.TrustExecution
	m.activeChat.TrustExecution = !oldValue
	for i := range m.workspace.Chats {
		if m.workspace.Chats[i].ID == m.activeChat.ID {
			m.workspace.Chats[i] = m.activeChat
			break
		}
	}
	if err := store.SaveWorkspace(m.workspace); err != nil {
		m.activeChat.TrustExecution = oldValue
		for i := range m.workspace.Chats {
			if m.workspace.Chats[i].ID == m.activeChat.ID {
				m.workspace.Chats[i].TrustExecution = oldValue
				break
			}
		}
		m.chatError = fmt.Sprintf("Save trusted execution setting: %v", err)
		return
	}
	m.chatError = ""
}

func (m *Model) toggleToolsMode() {
	oldValue := m.activeChat.ToolsEnabled
	m.activeChat.ToolsEnabled = !oldValue
	for i := range m.workspace.Chats {
		if m.workspace.Chats[i].ID == m.activeChat.ID {
			m.workspace.Chats[i] = m.activeChat
			break
		}
	}
	if err := store.SaveWorkspace(m.workspace); err != nil {
		m.activeChat.ToolsEnabled = oldValue
		for i := range m.workspace.Chats {
			if m.workspace.Chats[i].ID == m.activeChat.ID {
				m.workspace.Chats[i].ToolsEnabled = oldValue
				break
			}
		}
		m.chatError = fmt.Sprintf("Save tools mode setting: %v", err)
		return
	}
	m.chatError = ""
}

func (m *Model) cycleNewChatAgent(step int) {
	if len(m.workspace.Agents) == 0 {
		return
	}
	m.newChatAgent = (m.newChatAgent + step + len(m.workspace.Agents)) % len(m.workspace.Agents)
}

func (m *Model) createChat() error {
	if len(m.workspace.Agents) == 0 {
		return fmt.Errorf("create an agent before starting a chat")
	}
	title := strings.TrimSpace(m.chatTitleInput.Value())
	if title == "" {
		title = "New chat"
	}
	id, err := store.NewID()
	if err != nil {
		return err
	}
	agent := m.workspace.Agents[m.newChatAgent]
	chat := store.Chat{
		ID:        id,
		Title:     title,
		AgentID:   agent.ID,
		Thinking:  true,
		UpdatedAt: time.Now(),
	}
	m.workspace.Chats = append(m.workspace.Chats, chat)
	if err := store.SaveWorkspace(m.workspace); err != nil {
		m.workspace.Chats = m.workspace.Chats[:len(m.workspace.Chats)-1]
		return err
	}
	m.chatTitleInput.Blur()
	m.agentError = ""
	return m.openChat(chat)
}

func (m *Model) beginAgentEditor(agent *store.Agent) {
	m.state = stateEditAgent
	m.agentError = ""
	m.list.ResetFilter()
	m.editingAgentID = ""
	m.agentNameInput.SetValue("")
	m.agentPrompt.SetValue("")
	if agent != nil {
		m.editingAgentID = agent.ID
		m.agentNameInput.SetValue(agent.Name)
		m.agentPrompt.SetValue(agent.SystemPrompt)
	}
	m.agentNameInput.Focus()
	m.agentPrompt.Blur()
}

func (m *Model) saveAgent() error {
	name := strings.TrimSpace(m.agentNameInput.Value())
	if name == "" {
		return fmt.Errorf("agent name cannot be empty")
	}
	for _, agent := range m.workspace.Agents {
		if agent.ID != m.editingAgentID && strings.EqualFold(agent.Name, name) {
			return fmt.Errorf("an agent named %q already exists", name)
		}
	}

	original := store.Workspace{
		Agents: append([]store.Agent(nil), m.workspace.Agents...),
		Chats:  append([]store.Chat(nil), m.workspace.Chats...),
	}
	updated := false
	for i := range m.workspace.Agents {
		if m.workspace.Agents[i].ID == m.editingAgentID {
			m.workspace.Agents[i].Name = name
			m.workspace.Agents[i].SystemPrompt = m.agentPrompt.Value()
			updated = true
			break
		}
	}
	if !updated {
		id, err := store.NewID()
		if err != nil {
			return err
		}
		m.workspace.Agents = append(m.workspace.Agents, store.Agent{
			ID:           id,
			Name:         name,
			SystemPrompt: m.agentPrompt.Value(),
		})
	}
	if err := store.SaveWorkspace(m.workspace); err != nil {
		m.workspace = original
		return err
	}
	m.agentNameInput.Blur()
	m.agentPrompt.Blur()
	m.state = stateAgents
	m.agentError = ""
	m.list.Title = "Monya — Agents"
	m.list.SetItems(m.agentListItems())
	return nil
}

func (m *Model) updateChats(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok && m.list.FilterState() != list.Filtering {
		switch keyMsg.String() {
		case "q":
			m.closeTerminalSessions()
			return m, tea.Quit
		case "r":
			m.busy = true
			m.err = nil
			m.list.StartSpinner()
			return m, loadWorkspace(m.cfg.DefaultAgent)
		case "a":
			m.state = stateAgents
			m.err = nil
			m.agentError = ""
			m.list.Title = "Monya — Agents"
			m.list.ResetFilter()
			return m, m.list.SetItems(m.agentListItems())
		case "t":
			m.terminalReturnState = stateChats
			m.state = stateTerminals
			m.list.Title = "Monya — Terminals"
			m.list.ResetFilter()
			m.terminalNotice = ""
			return m, m.list.SetItems(m.terminalListItems())
		case "n":
			m.state = stateNewChat
			m.chatTitleInput.SetValue("")
			m.chatTitleInput.Focus()
			m.newChatAgent = 0
			m.agentError = ""
			return m, textinput.Blink
		case "d":
			if item, ok := m.list.SelectedItem().(chatItem); ok {
				m.deleteChatID = item.chat.ID
				m.deleteChatName = item.chat.Title
				m.state = stateConfirmDeleteChat
				m.agentError = ""
				return m, nil
			}
		case "enter":
			if item, ok := m.list.SelectedItem().(chatItem); ok {
				if err := m.openChat(item.chat); err != nil {
					m.err = err
					return m, nil
				}
				return m, textarea.Blink
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *Model) openTerminalScreen(entry *terminalEntry) {
	m.activeTerminalID = entry.id
	m.state = stateTerminal
	m.terminalNotice = ""
	m.textarea.SetValue("")
	m.textarea.Placeholder = "Enter a shell command..."
	m.textarea.Focus()
	m.updateTerminalViewport(entry)
}

func (m *Model) createTerminal() tea.Cmd {
	id := fmt.Sprintf("terminal-%d", m.nextTerminalID)
	name := fmt.Sprintf("Terminal %d", m.nextTerminalID)
	m.nextTerminalID++
	entry := &terminalEntry{id: id, name: name, starting: true}
	m.terminals = append(m.terminals, entry)
	m.openTerminalScreen(entry)
	return m.startTerminalSession(id)
}

func (m *Model) updateTerminalList(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok && m.list.FilterState() != list.Filtering {
		switch keyMsg.String() {
		case "esc":
			m.state = m.terminalReturnState
			m.list.Title = "Monya — Chats"
			m.list.ResetFilter()
			m.terminalNotice = ""
			if m.state == stateChat {
				m.textarea.Placeholder = "Type a message..."
				m.textarea.Focus()
				m.updateViewport()
			}
			return m, m.list.SetItems(m.chatListItems())
		case "n":
			return m, m.createTerminal()
		case "enter":
			if item, ok := m.list.SelectedItem().(terminalItem); ok {
				m.openTerminalScreen(item.terminal)
				return m, nil
			}
		case "s":
			if item, ok := m.list.SelectedItem().(terminalItem); ok {
				entry := item.terminal
				if entry.starting || entry.session == nil {
					m.terminalNotice = "This terminal is not ready to be selected."
					return m, nil
				}
				m.activeTerminalID = entry.id
				m.state = m.terminalReturnState
				m.list.Title = "Monya — Chats"
				m.list.ResetFilter()
				m.terminalNotice = ""
				if m.state == stateChat {
					m.textarea.Placeholder = "Type a message..."
					m.textarea.Focus()
					m.updateViewport()
				}
				return m, m.list.SetItems(m.chatListItems())
			}
		case "d":
			if item, ok := m.list.SelectedItem().(terminalItem); ok {
				entry := item.terminal
				if len(m.terminals) <= 1 {
					m.terminalNotice = "Keep at least one terminal session."
					return m, nil
				}
				if entry.starting || m.commandRunning && m.commandTerminalID == entry.id {
					m.terminalNotice = "This terminal is still starting or running a command."
					return m, nil
				}
				m.deleteTerminalID = entry.id
				m.deleteTerminalName = entry.name
				m.state = stateConfirmCloseTerminal
				m.terminalNotice = ""
				return m, nil
			}
		case "r":
			if item, ok := m.list.SelectedItem().(terminalItem); ok {
				entry := item.terminal
				if entry.starting || m.commandRunning && m.commandTerminalID == entry.id {
					m.terminalNotice = "This terminal is still starting or running a command."
					return m, nil
				}
				if entry.session != nil {
					m.terminalNotice = "Close the terminal before restarting it."
					return m, nil
				}
				entry.starting = true
				entry.err = ""
				m.terminalNotice = ""
				return m, m.startTerminalSession(entry.id)
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *Model) updateTerminalScreen(msg tea.Msg) (tea.Model, tea.Cmd) {
	if mouseMsg, ok := msg.(tea.MouseMsg); ok {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(mouseMsg)
		return m, cmd
	}
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		if m.commandTimedOut {
			m.handleCommandTimeoutKey(keyMsg)
			return m, nil
		}
		if m.commandRunning {
			if keyMsg.String() == "enter" {
				if input := m.textarea.Value(); input != "" {
					if m.terminalInputPending {
						m.terminalNotice = "Input is still being sent."
						return m, nil
					}
					entry := m.terminalByID(m.commandTerminalID)
					if entry == nil || entry.session == nil {
						m.terminalNotice = "The running terminal is unavailable."
					} else {
						m.textarea.Reset()
						m.terminalInputPending = true
						m.terminalNotice = "Sending input..."
						return m, sendTerminalInput(entry.id, entry.session, input)
					}
					m.textarea.Reset()
				}
				return m, nil
			}
			if keyMsg.String() == "esc" {
				if m.terminalReturnState == stateChat {
					m.state = stateChat
					m.textarea.Placeholder = "Input to the running terminal..."
					m.textarea.Focus()
					m.updateViewport()
				} else {
					m.terminalNotice = "Command is running; enter input or wait for it to finish."
				}
				return m, nil
			}
		} else {
			switch keyMsg.String() {
			case "esc":
				m.state = stateTerminals
				m.textarea.Blur()
				m.list.Title = "Monya — Terminals"
				m.list.ResetFilter()
				return m, m.list.SetItems(m.terminalListItems())
			case "enter":
				command := strings.TrimSpace(m.textarea.Value())
				if command == "" {
					return m, nil
				}
				entry := m.activeTerminal()
				if entry == nil || entry.session == nil {
					m.terminalNotice = "Terminal session is not ready."
					return m, nil
				}
				m.appendTerminalOutput(entry, "$ "+command)
				m.textarea.Reset()
				m.commandRunning = true
				m.commandTerminalID = entry.id
				m.commandTimeoutID++
				timeoutID := m.commandTimeoutID
				m.terminalNotice = ""
				m.updateTerminalViewport(entry)
				return m, tea.Batch(
					executeTerminalCommand(entry.id, entry.session, command, false),
					terminalOutputTick(entry.id),
					tea.Tick(30*time.Second, func(time.Time) tea.Msg {
						return commandTimeoutMsg{token: timeoutID}
					}),
				)
			}
		}
		if keyMsg.String() == "pgup" || keyMsg.String() == "pgdown" {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(keyMsg)
			return m, cmd
		}
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m *Model) handleCommandTimeoutKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "k", "K":
		entry := m.terminalByID(m.commandTerminalID)
		if entry == nil || entry.session == nil {
			m.terminalNotice = "The running terminal is unavailable."
		} else if err := entry.session.Kill(); err != nil {
			m.terminalNotice = err.Error()
		} else {
			m.terminalNotice = "Terminal stopped; restart it from the terminal list."
		}
		m.commandTimedOut = false
	case "w", "W":
		m.commandTimedOut = false
	}
}

func (m *Model) updateNewChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.chatTitleInput, cmd = m.chatTitleInput.Update(msg)
		return m, cmd
	}
	switch keyMsg.String() {
	case "esc":
		m.state = stateChats
		m.chatTitleInput.Blur()
		m.agentError = ""
		return m, nil
	case "left", "shift+tab":
		m.cycleNewChatAgent(-1)
		return m, nil
	case "right", "tab":
		m.cycleNewChatAgent(1)
		return m, nil
	case "enter":
		if err := m.createChat(); err != nil {
			m.agentError = err.Error()
			return m, nil
		}
		return m, textarea.Blink
	}
	var cmd tea.Cmd
	m.chatTitleInput, cmd = m.chatTitleInput.Update(msg)
	return m, cmd
}

func (m *Model) updateAgents(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok && m.list.FilterState() != list.Filtering {
		switch keyMsg.String() {
		case "esc":
			m.state = stateChats
			m.list.Title = "Monya — Chats"
			m.agentError = ""
			m.list.ResetFilter()
			return m, m.list.SetItems(m.chatListItems())
		case "n":
			m.beginAgentEditor(nil)
			return m, textinput.Blink
		case "enter":
			if item, ok := m.list.SelectedItem().(agentItem); ok {
				agent := item.agent
				m.beginAgentEditor(&agent)
				return m, textinput.Blink
			}
		case "d":
			if item, ok := m.list.SelectedItem().(agentItem); ok {
				for _, chat := range m.workspace.Chats {
					if chat.AgentID == item.agent.ID {
						m.agentError = "Cannot delete an agent while chats use it; delete those chats first."
						return m, nil
					}
				}
				m.deleteAgentID = item.agent.ID
				m.state = stateConfirmDeleteAgent
				m.agentError = ""
				m.list.ResetFilter()
				return m, nil
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *Model) updateAgentEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "esc":
			m.state = stateAgents
			m.agentNameInput.Blur()
			m.agentPrompt.Blur()
			m.agentError = ""
			return m, nil
		case "ctrl+s":
			if err := m.saveAgent(); err != nil {
				m.agentError = err.Error()
				return m, nil
			}
			return m, nil
		case "tab", "shift+tab":
			if m.agentPrompt.Focused() {
				m.agentPrompt.Blur()
				m.agentNameInput.Focus()
			} else {
				m.agentNameInput.Blur()
				m.agentPrompt.Focus()
			}
			return m, nil
		case "enter":
			if !m.agentPrompt.Focused() {
				m.agentNameInput.Blur()
				m.agentPrompt.Focus()
				return m, nil
			}
		}
	}
	if m.agentPrompt.Focused() {
		var cmd tea.Cmd
		m.agentPrompt, cmd = m.agentPrompt.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.agentNameInput, cmd = m.agentNameInput.Update(msg)
	return m, cmd
}

func (m *Model) updateDeleteAgent(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "esc", "n", "N":
		m.state = stateAgents
		m.deleteAgentID = ""
		return m, nil
	case "y", "Y":
		original := append([]store.Agent(nil), m.workspace.Agents...)
		filtered := m.workspace.Agents[:0]
		for _, agent := range m.workspace.Agents {
			if agent.ID != m.deleteAgentID {
				filtered = append(filtered, agent)
			}
		}
		m.workspace.Agents = filtered
		if err := store.SaveWorkspace(m.workspace); err != nil {
			m.workspace.Agents = original
			m.agentError = fmt.Sprintf("Delete agent: %v", err)
		} else {
			m.agentError = ""
		}
		m.deleteAgentID = ""
		m.state = stateAgents
		m.list.SetItems(m.agentListItems())
	}
	return m, nil
}

func (m *Model) updateDeleteChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "esc", "n", "N":
		m.state = stateChats
		m.deleteChatID = ""
		m.deleteChatName = ""
		return m, nil
	case "y", "Y":
		original := append([]store.Chat(nil), m.workspace.Chats...)
		filtered := m.workspace.Chats[:0]
		for _, chat := range m.workspace.Chats {
			if chat.ID != m.deleteChatID {
				filtered = append(filtered, chat)
			}
		}
		m.workspace.Chats = filtered
		if err := store.SaveWorkspace(m.workspace); err != nil {
			m.workspace.Chats = original
			m.err = fmt.Errorf("delete chat: %w", err)
		} else if err := store.DeleteChatHistory(m.deleteChatID); err != nil {
			m.workspace.Chats = original
			if restoreErr := store.SaveWorkspace(m.workspace); restoreErr != nil {
				m.err = fmt.Errorf("delete chat history: %v; restore chat list: %w", err, restoreErr)
			} else {
				m.err = err
			}
		} else {
			m.err = nil
		}
		m.deleteChatID = ""
		m.deleteChatName = ""
		m.state = stateChats
		return m, m.list.SetItems(m.chatListItems())
	}
	return m, nil
}

func (m *Model) updateCloseTerminal(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "esc", "n", "N":
		m.state = stateTerminals
		m.deleteTerminalID = ""
		m.deleteTerminalName = ""
	case "y", "Y":
		entry := m.terminalByID(m.deleteTerminalID)
		if entry != nil && entry.session != nil {
			if err := entry.session.Close(); err != nil {
				m.terminalNotice = err.Error()
				m.state = stateTerminals
				m.deleteTerminalID = ""
				m.deleteTerminalName = ""
				return m, nil
			}
		}
		filtered := m.terminals[:0]
		for _, terminal := range m.terminals {
			if terminal.id != m.deleteTerminalID {
				filtered = append(filtered, terminal)
			}
		}
		m.terminals = filtered
		if m.activeTerminalID == m.deleteTerminalID {
			m.activeTerminalID = m.terminals[0].id
		}
		m.deleteTerminalID = ""
		m.deleteTerminalName = ""
		m.terminalNotice = ""
		m.state = stateTerminals
		return m, m.list.SetItems(m.terminalListItems())
	}
	return m, nil
}

func (m *Model) handleTerminalStarted(msg terminalStartedMsg) (tea.Model, tea.Cmd) {
	entry := m.terminalByID(msg.id)
	if entry == nil {
		if msg.session != nil {
			_ = msg.session.Close()
		}
		return m, nil
	}
	entry.starting = false
	if msg.err != nil {
		entry.err = msg.err.Error()
		m.appendTerminalOutput(entry, "Terminal startup failed: "+msg.err.Error())
	} else {
		entry.session = msg.session
		entry.cwd = msg.session.CWD()
		entry.err = ""
	}
	if m.state == stateTerminal && m.activeTerminalID == entry.id {
		m.updateTerminalViewport(entry)
	}
	if m.state == stateTerminals {
		return m, m.list.SetItems(m.terminalListItems())
	}
	return m, nil
}

func (m *Model) handleTerminalCommandDone(msg terminalCommandDoneMsg) tea.Cmd {
	entry := m.terminalByID(msg.terminalID)
	m.commandRunning = false
	m.commandTimedOut = false
	m.commandTerminalID = ""
	if m.state == stateTerminal {
		m.textarea.Placeholder = "Enter a shell command..."
	} else {
		m.textarea.Placeholder = "Type a message..."
	}

	content := msg.result.Output
	if msg.err != nil {
		if content != "" {
			content += "\n"
		}
		content += "[terminal error: " + msg.err.Error() + "]"
		if entry != nil {
			entry.err = msg.err.Error()
			if entry.session != nil {
				if closeErr := entry.session.Close(); closeErr != nil {
					entry.err += "; " + closeErr.Error()
					content += "\n[terminal cleanup error: " + closeErr.Error() + "]"
				}
			}
			entry.session = nil
		}
	} else if entry != nil {
		entry.cwd = msg.result.CWD
		entry.err = ""
		if msg.result.Status != 0 {
			if content != "" {
				content += "\n"
			}
			content += fmt.Sprintf("[exit status: %d]", msg.result.Status)
		}
	}
	if len(content) > 16384 {
		content = content[:16384] + "\n...[truncated]"
	}
	if entry != nil && msg.result.Output != "" {
		m.appendTerminalOutput(entry, msg.result.Output)
	}
	if entry != nil && msg.err != nil {
		m.appendTerminalOutput(entry, "[terminal error: "+msg.err.Error()+"]")
	} else if entry != nil && msg.result.Status != 0 {
		m.appendTerminalOutput(entry, fmt.Sprintf("[exit status: %d]", msg.result.Status))
	}
	if m.state == stateTerminal && entry != nil {
		m.updateTerminalViewport(entry)
	}
	if m.state == stateTerminals {
		m.list.SetItems(m.terminalListItems())
	}
	if msg.aiTool {
		m.state = stateChat
		m.textarea.Placeholder = "Type a message..."
		m.textarea.Focus()
		if len(m.history) > 0 && m.history[len(m.history)-1].Role == "tool" {
			m.history[len(m.history)-1].Content = content
		}
		m.persistActiveChat()
		m.updateViewport()
		return m.processNextToolCall()
	}
	m.terminalNotice = ""
	return nil
}

func (m *Model) processNextToolCall() tea.Cmd {
	for len(m.toolCallQueue) > 0 {
		call := m.toolCallQueue[0]
		m.toolCallQueue = m.toolCallQueue[1:]
		if call.Type == "" {
			call.Type = "function"
		}

		if call.Function.Name == "run_bash" {
			command, err := parseRunBashCommand(call.Function.Arguments)
			if err != nil {
				m.history = append(m.history, api.Message{
					Role:       "tool",
					ToolCallID: call.ID,
					Name:       call.Function.Name,
					Content:    "Tool call rejected: " + err.Error(),
				})
				m.persistActiveChat()
				m.updateViewport()
				continue
			}
			m.pendingToolCall = &call
			m.pendingCmd = command
			m.updateViewport()
			if m.shouldAutoRunCommand(command) {
				return m.executePendingToolCall()
			}
			return nil
		}

		if agenttools.IsFileTool(call.Function.Name) {
			m.fileToolRunning = true
			m.history = append(m.history, api.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Content:    "Working...",
			})
			m.persistActiveChat()
			m.updateViewport()
			return m.runFileTool(call)
		}

		m.history = append(m.history, api.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Name:       call.Function.Name,
			Content:    "Tool call rejected: unknown tool",
		})
		m.persistActiveChat()
		m.updateViewport()
	}

	m.persistActiveChat()
	m.updateViewport()
	return m.startStream()
}

func (m *Model) executePendingToolCall() tea.Cmd {
	if m.pendingToolCall == nil {
		return nil
	}
	cmdStr := m.pendingCmd
	tcID := m.pendingToolCall.ID
	entry := m.activeTerminal()
	m.history = append(m.history, api.Message{
		Role:       "tool",
		ToolCallID: tcID,
		Name:       "run_bash",
		Content:    "Executing...",
	})
	m.pendingToolCall = nil
	m.commandTimedOut = false
	if entry == nil || entry.session == nil {
		m.history[len(m.history)-1].Content = "No active terminal session is available."
		m.persistActiveChat()
		m.updateViewport()
		return m.processNextToolCall()
	}
	m.commandRunning = true
	m.commandTerminalID = entry.id
	m.commandTimeoutID++
	timeoutID := m.commandTimeoutID
	m.textarea.SetValue("")
	m.textarea.Placeholder = "Input to the running terminal..."
	m.appendTerminalOutput(entry, "[Monya] $ "+cmdStr)
	m.persistActiveChat()
	execCmd := executeTerminalCommand(entry.id, entry.session, cmdStr, true)
	timerCmd := tea.Tick(30*time.Second, func(time.Time) tea.Msg {
		return commandTimeoutMsg{token: timeoutID}
	})
	m.updateViewport()
	return tea.Batch(execCmd, terminalOutputTick(entry.id), timerCmd)
}

func (m *Model) runFileTool(call api.ToolCall) tea.Cmd {
	rootPath := m.workspaceRoot
	rootErr := m.workspaceRootErr
	return func() tea.Msg {
		if rootErr != nil {
			return fileToolDoneMsg{
				callID: call.ID,
				name:   call.Function.Name,
				err:    fmt.Errorf("determine project root: %w", rootErr),
			}
		}
		result, err := agenttools.Execute(rootPath, call.Function.Name, call.Function.Arguments)
		return fileToolDoneMsg{
			callID: call.ID,
			name:   call.Function.Name,
			result: result,
			err:    err,
		}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = max(1, msg.Width-2)
		statusRows := lipgloss.Height(m.chatStatusLine())
		m.viewport.Height = max(1, msg.Height-10-statusRows)
		m.textarea.SetWidth(max(1, msg.Width-4))
		m.agentPrompt.SetWidth(max(1, msg.Width-4))
		m.agentPrompt.SetHeight(max(1, msg.Height-12))
		m.list.SetSize(max(1, msg.Width), max(1, msg.Height-5)) // leave space for custom footer
		if m.state == stateTerminal {
			if entry := m.activeTerminal(); entry != nil {
				m.updateTerminalViewport(entry)
			}
		} else {
			m.updateViewport()
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.commandRunning {
				entry := m.terminalByID(m.commandTerminalID)
				if entry == nil || entry.session == nil {
					m.terminalNotice = "The running terminal is unavailable."
				} else if err := entry.session.Interrupt(); err != nil {
					m.terminalNotice = err.Error()
				} else {
					m.terminalNotice = "Interrupt sent to the running command."
				}
				m.commandTimedOut = false
				return m, nil
			}
			if m.isStreaming && m.cancelStream != nil {
				m.cancelStream()
			}
			m.closeTerminalSessions()
			return m, tea.Quit
		}

		if msg.String() == "f2" {
			m.mouseOn = !m.mouseOn
			if m.mouseOn {
				return m, func() tea.Msg { return tea.EnableMouseCellMotion() }
			}
			return m, func() tea.Msg { return tea.DisableMouse() }
		}

	case workspaceLoadedMsg:
		m.busy = false
		m.list.StopSpinner()
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.workspace = msg.workspace
		m.approvedCommands = make(map[string]struct{}, len(msg.approvedCommands))
		for _, command := range msg.approvedCommands {
			command = strings.TrimSpace(command)
			if command != "" {
				m.approvedCommands[command] = struct{}{}
			}
		}
		m.err = nil
		m.state = stateChats
		m.list.Title = "Monya — Chats"
		m.list.ResetFilter()
		return m, m.list.SetItems(m.chatListItems())

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case clearCopyStatusMsg:
		if msg.token == m.copyStatusToken {
			m.copyStatus = ""
		}
		return m, nil

	case streamStartedMsg:
		m.stream = msg.stream
		m.currentMaxTokens = msg.maxTokens
		if msg.contextTrimmed > 0 {
			m.contextNotice = fmt.Sprintf("Context compacted: omitted %d older messages from this request", msg.contextTrimmed)
		} else {
			m.contextNotice = ""
		}
		return m, m.readNextChunk()

	case terminalStartedMsg:
		return m.handleTerminalStarted(msg)

	case terminalCommandDoneMsg:
		return m, m.handleTerminalCommandDone(msg)

	case fileToolDoneMsg:
		last := len(m.history) - 1
		if last < 0 || m.history[last].Role != "tool" ||
			m.history[last].ToolCallID != msg.callID || m.history[last].Name != msg.name {
			m.chatError = fmt.Sprintf("Unexpected result from file tool %q", msg.name)
			return m, nil
		}
		m.fileToolRunning = false
		content := msg.result
		if msg.err != nil {
			content = "File tool failed: " + msg.err.Error()
		}
		m.history[last].Content = content
		return m, m.processNextToolCall()

	case terminalInputDoneMsg:
		m.terminalInputPending = false
		entry := m.terminalByID(msg.terminalID)
		if msg.err != nil {
			m.terminalNotice = msg.err.Error()
			return m, nil
		}
		if entry != nil {
			m.appendTerminalOutput(entry, "> "+msg.input)
			if m.state == stateTerminal {
				m.updateTerminalViewport(entry)
			}
		}
		m.terminalNotice = ""
		return m, nil

	case terminalOutputTickMsg:
		if !m.commandRunning || msg.terminalID != m.commandTerminalID {
			return m, nil
		}
		entry := m.terminalByID(msg.terminalID)
		if entry != nil && entry.session != nil {
			m.appendTerminalOutput(entry, entry.session.DrainOutput())
			if m.state == stateTerminal {
				m.updateTerminalViewport(entry)
			}
		}
		return m, terminalOutputTick(msg.terminalID)

	case commandTimeoutMsg:
		if m.commandRunning && msg.token == m.commandTimeoutID {
			m.commandTimedOut = true
		}
		return m, nil
	}

	// Routing
	switch m.state {
	case stateChats:
		return m.updateChats(msg)
	case stateNewChat:
		return m.updateNewChat(msg)
	case stateAgents:
		return m.updateAgents(msg)

	case stateEditAgent:
		return m.updateAgentEditor(msg)

	case stateConfirmDeleteAgent:
		return m.updateDeleteAgent(msg)
	case stateConfirmDeleteChat:
		return m.updateDeleteChat(msg)
	case stateTerminals:
		return m.updateTerminalList(msg)
	case stateTerminal:
		return m.updateTerminalScreen(msg)
	case stateConfirmCloseTerminal:
		return m.updateCloseTerminal(msg)

	case stateChat:
		switch msg := msg.(type) {
		case tea.MouseMsg:
			if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
				if code, ok := m.codeBlockAtScreenY(msg.Y); ok {
					return m, m.copyText(code, "Code block copied to clipboard")
				}
			}
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd

		case tea.KeyMsg:
			if m.commandTimedOut {
				m.handleCommandTimeoutKey(msg)
				return m, nil
			}
			if m.fileToolRunning {
				if msg.String() == "pgup" || msg.String() == "pgdown" {
					var cmd tea.Cmd
					m.viewport, cmd = m.viewport.Update(msg)
					return m, cmd
				}
				return m, nil
			}

			if m.pendingToolCall != nil {
				switch msg.String() {
				case "y", "Y", "enter":
					var rememberErr error
					if m.activeChat.TrustExecution && isRememberableCommand(m.pendingCmd) {
						rememberErr = m.rememberApprovedCommand(m.pendingCmd)
					}
					cmd := m.executePendingToolCall()
					if rememberErr != nil {
						m.chatError = fmt.Sprintf("Save approved command: %v", rememberErr)
					}
					return m, cmd
				case "n", "N", "esc":
					tcID := m.pendingToolCall.ID
					m.history = append(m.history, api.Message{
						Role:       "tool",
						ToolCallID: tcID,
						Name:       "run_bash",
						Content:    "User denied execution.",
					})
					m.pendingToolCall = nil
					m.persistActiveChat()
					return m, m.processNextToolCall()
				}
				return m, nil
			}

			if m.commandRunning {
				if msg.String() == "ctrl+t" {
					m.terminalReturnState = stateChat
					m.state = stateTerminal
					m.textarea.Placeholder = "Input to the running terminal..."
					m.textarea.SetValue("")
					if entry := m.terminalByID(m.commandTerminalID); entry != nil {
						m.updateTerminalViewport(entry)
					}
					return m, nil
				}
				if msg.String() == "enter" {
					if input := m.textarea.Value(); input != "" {
						if m.terminalInputPending {
							m.terminalNotice = "Input is still being sent."
							return m, nil
						}
						entry := m.terminalByID(m.commandTerminalID)
						if entry == nil || entry.session == nil {
							m.terminalNotice = "The running terminal is unavailable."
						} else {
							m.textarea.Reset()
							m.terminalInputPending = true
							m.terminalNotice = "Sending input..."
							return m, sendTerminalInput(entry.id, entry.session, input)
						}
						m.textarea.Reset()
					}
					return m, nil
				}
				if msg.String() == "pgup" || msg.String() == "pgdown" {
					var cmd tea.Cmd
					m.viewport, cmd = m.viewport.Update(msg)
					return m, cmd
				}
				var cmd tea.Cmd
				m.textarea, cmd = m.textarea.Update(msg)
				return m, cmd
			}

			if msg.String() == "ctrl+y" {
				return m, m.copyLastAnswer()
			}
			if msg.String() == "ctrl+b" {
				return m, m.copyLastCode()
			}
			if msg.String() == "f3" && !m.isStreaming && m.pendingToolCall == nil {
				oldThinking := m.activeChat.Thinking
				m.activeChat.Thinking = !m.activeChat.Thinking
				for i := range m.workspace.Chats {
					if m.workspace.Chats[i].ID == m.activeChat.ID {
						m.workspace.Chats[i] = m.activeChat
						break
					}
				}
				if err := store.SaveWorkspace(m.workspace); err != nil {
					m.activeChat.Thinking = oldThinking
					for i := range m.workspace.Chats {
						if m.workspace.Chats[i].ID == m.activeChat.ID {
							m.workspace.Chats[i].Thinking = oldThinking
							break
						}
					}
					m.chatError = fmt.Sprintf("Save thinking setting: %v", err)
				} else {
					m.chatError = ""
				}
				return m, nil
			}
			if msg.String() == "f4" && !m.isStreaming && m.pendingToolCall == nil {
				m.toggleTrustExecution()
				return m, nil
			}
			if msg.String() == "f5" && !m.isStreaming && !m.commandRunning &&
				!m.fileToolRunning && m.pendingToolCall == nil {
				m.toggleToolsMode()
				return m, nil
			}
			if msg.String() == "ctrl+t" && !m.isStreaming {
				m.terminalReturnState = stateChat
				m.state = stateTerminals
				m.textarea.Blur()
				m.list.Title = "Monya — Terminals"
				m.list.ResetFilter()
				m.terminalNotice = ""
				return m, m.list.SetItems(m.terminalListItems())
			}

			switch msg.String() {
			case "esc":
				if m.isStreaming {
					if m.cancelStream != nil {
						m.cancelStream()
					}
					m.isStreaming = false
					m.finalAnswerRetry = false
					m.finalAnswerTokens = 0
					m.updateViewport()
					return m, nil
				}
				m.state = stateChats
				m.textarea.Blur()
				m.textarea.Placeholder = "Type a message..."
				m.list.Title = "Monya — Chats"
				return m, m.list.SetItems(m.chatListItems())
			case "enter":
				text := strings.TrimSpace(m.textarea.Value())
				if text != "" && !m.isStreaming {
					m.finalAnswerRetry = false
					m.finalAnswerTokens = 0
					m.history = append(m.history, api.Message{Role: "user", Content: text})
					m.persistActiveChat()
					m.textarea.Reset()
					return m, m.startStream()
				}
				return m, nil
			}

			if msg.String() == "pgup" || msg.String() == "pgdown" {
				var cmd tea.Cmd
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}

			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd

		case chunkMsg:
			if len(msg.Choices) > 0 {
				d := msg.Choices[0].Delta
				if finishReason := msg.Choices[0].FinishReason; finishReason != nil {
					m.currentFinishReason = *finishReason
				}
				if d.ReasoningContent != "" {
					m.currentReasoning += d.ReasoningContent
				}
				if d.Content != "" {
					m.currentResponse += d.Content
				}
				if len(d.ToolCalls) > 0 {
					m.currentToolCalls = mergeToolCallDeltas(m.currentToolCalls, d.ToolCalls)
				}
				m.updateViewport()
			}
			return m, m.readNextChunk()

		case streamDoneMsg:
			m.isStreaming = false
			if m.stream != nil {
				m.stream.Close()
				m.stream = nil
			}
			if m.cancelStream != nil {
				m.cancelStream()
				m.cancelStream = nil
			}

			if len(m.currentToolCalls) > 0 && !m.activeChat.ToolsEnabled {
				content := strings.TrimSpace(m.currentResponse)
				if content != "" {
					content += "\n\n"
				}
				content += "Tools are disabled for this chat. Press F5 to enable them, then ask again."
				m.history = append(m.history, api.Message{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: m.currentReasoning,
				})
				m.persistActiveChat()
				m.currentReasoning = ""
				m.currentResponse = ""
				m.currentFinishReason = ""
				m.currentToolCalls = nil
				m.finalAnswerRetry = false
				m.finalAnswerTokens = 0
				m.updateViewport()
				return m, nil
			}

			if len(m.currentToolCalls) > 0 {
				m.finalAnswerRetry = false
				m.finalAnswerTokens = 0
				for i := range m.currentToolCalls {
					if m.currentToolCalls[i].Type == "" {
						m.currentToolCalls[i].Type = "function"
					}
				}
				m.history = append(m.history, api.Message{
					Role:             "assistant",
					Content:          m.currentResponse,
					ReasoningContent: m.currentReasoning,
					ToolCalls:        m.currentToolCalls,
				})
				m.persistActiveChat()
				m.toolCallQueue = append(m.toolCallQueue[:0], m.currentToolCalls...)
				m.currentReasoning = ""
				m.currentResponse = ""
				m.currentFinishReason = ""
				m.currentToolCalls = nil
				return m, m.processNextToolCall()
			}
			if strings.TrimSpace(m.currentResponse) == "" {
				if !m.finalAnswerRetry && m.currentFinishReason != "length" {
					m.finalAnswerRetry = true
					m.finalAnswerTokens = maxResponseTokens
					if m.currentMaxTokens > m.finalAnswerTokens {
						m.finalAnswerTokens = m.currentMaxTokens
					}
					m.currentReasoning = ""
					m.currentFinishReason = ""
					m.updateViewport()
					return m, m.startStream()
				}
				m.finalAnswerRetry = false
				m.finalAnswerTokens = 0
				m.currentReasoning = ""
				if m.currentFinishReason == "length" {
					m.chatError = "Model reached the response token limit without producing an answer. No repeat request was sent."
				} else {
					m.chatError = "Model finished without producing an answer after retry. Try sending the request again."
				}
				m.persistActiveChat()
				m.updateViewport()
				return m, nil
			}
			m.history = append(m.history, api.Message{
				Role:             "assistant",
				Content:          m.currentResponse,
				ReasoningContent: m.currentReasoning,
			})
			m.persistActiveChat()
			m.currentReasoning = ""
			m.currentResponse = ""
			m.currentFinishReason = ""
			m.finalAnswerRetry = false
			m.finalAnswerTokens = 0
			m.updateViewport()
			return m, nil

		case streamErrMsg:
			m.isStreaming = false
			m.chatError = fmt.Sprintf("Error: %v", msg.err)
			m.finalAnswerRetry = false
			m.finalAnswerTokens = 0
			if m.stream != nil {
				m.stream.Close()
				m.stream = nil
			}
			if m.cancelStream != nil {
				m.cancelStream()
				m.cancelStream = nil
			}
			m.updateViewport()
			return m, nil

		}
	}

	return m, nil
}

var (
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).MarginTop(1)
	userStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	reasoningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
	toolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

func (m *Model) View() string {
	switch m.state {
	case stateChats:
		if m.err != nil {
			return fmt.Sprintf("\n  %s\n\n  %s\n", errStyle.Render("Error: "+m.err.Error()), helpStyle.Render("[r]etry  [q]uit"))
		}
		footer := helpStyle.Render("[↑/↓] navigate  [/] search  [enter] open  [n] new  [d] delete  [a] agents  [t] terminals  [r] reload  [q] quit")
		terminalStatus := dimStyle.Render("Active terminal for Monya: " + m.activeTerminalName())
		return "\n" + m.list.View() + "\n" + footer + "\n" + terminalStatus
	case stateChat:
		return m.viewChat()
	case stateNewChat:
		return m.viewNewChat()
	case stateAgents:
		var b strings.Builder
		b.WriteString("\n" + m.list.View() + "\n")
		b.WriteString(helpStyle.Render("[enter] edit  [n] new agent  [d] delete  [esc] chats"))
		if m.agentError != "" {
			b.WriteString("\n" + errStyle.Render(m.agentError))
		}
		return b.String()
	case stateEditAgent:
		return m.viewAgentEditor()
	case stateConfirmDeleteAgent:
		agentName := m.agentName(m.deleteAgentID)
		return "\n" + fmt.Sprintf("Delete agent %q?", agentName) + "\n\n" + helpStyle.Render("[y] delete  [n/esc] cancel")
	case stateConfirmDeleteChat:
		return "\n" + fmt.Sprintf("Delete chat %q and its history?", m.deleteChatName) + "\n\n" + helpStyle.Render("[y] delete  [n/esc] cancel")
	case stateTerminals:
		var b strings.Builder
		b.WriteString("\n" + m.list.View() + "\n")
		b.WriteString(helpStyle.Render("[s] select for Monya without opening  [enter] open  [n] new  [d] close  [r] restart unavailable  [esc] back"))
		if m.terminalNotice != "" {
			b.WriteString("\n" + errStyle.Render(m.terminalNotice))
		}
		return b.String()
	case stateTerminal:
		return m.viewTerminal()
	case stateConfirmCloseTerminal:
		return "\n" + fmt.Sprintf("Close terminal %q and stop its shell?", m.deleteTerminalName) + "\n\n" + helpStyle.Render("[y] close  [n/esc] cancel")
	default:
		return "Unknown state\n"
	}
}

func (m *Model) viewTerminal() string {
	var b strings.Builder
	entry := m.activeTerminal()
	name := "Unknown terminal"
	cwd := ""
	switch {
	case entry == nil:
		b.WriteString(errStyle.Render("No terminal is selected."))
	case entry.starting:
		name = entry.name
		b.WriteString(m.spinner.View() + " Starting shell...")
	case entry.err != "":
		name = entry.name
		b.WriteString(errStyle.Render("Shell unavailable: " + entry.err))
	default:
		name = entry.name
		cwd = entry.cwd
	}
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(name))
	if cwd != "" {
		b.WriteString(dimStyle.Render(" · " + cwd))
	}
	b.WriteString("\n\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	if m.commandTimedOut {
		b.WriteString(errStyle.Render("Command is taking too long (>30s)."))
		b.WriteString("\n" + helpStyle.Render("[K]ill  [W]ait"))
	} else if m.commandRunning {
		b.WriteString(dimStyle.Render("Command is running; Enter sends input to the shell."))
		b.WriteString("\n" + m.textarea.View())
		help := "[enter] send input  [ctrl+j] newline"
		if m.terminalReturnState == stateChat {
			help += "  [esc] chat"
		}
		b.WriteString("\n" + helpStyle.Render(help))
	} else {
		b.WriteString(m.textarea.View())
		b.WriteString("\n" + helpStyle.Render("[enter] run command  [ctrl+j] newline  [pgup/pgdown] scroll  [esc] terminal list"))
	}
	if m.terminalNotice != "" {
		b.WriteString("\n" + errStyle.Render(m.terminalNotice))
	}
	return b.String()
}

func (m *Model) viewNewChat() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("Create chat"))
	b.WriteString("\n\n")
	b.WriteString("Title\n")
	b.WriteString(m.chatTitleInput.View())
	b.WriteString("\n\nAgent: ")
	if len(m.workspace.Agents) > 0 && m.newChatAgent < len(m.workspace.Agents) {
		b.WriteString(m.workspace.Agents[m.newChatAgent].Name)
		b.WriteString(helpStyle.Render("  [←/→ or tab] change"))
	} else {
		b.WriteString("No agents available")
	}
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("[enter] create  [esc] cancel"))
	if m.agentError != "" {
		b.WriteString("\n" + errStyle.Render(m.agentError))
	}
	return b.String()
}

func (m *Model) viewAgentEditor() string {
	var b strings.Builder
	title := "New agent"
	if m.editingAgentID != "" {
		title = "Edit agent"
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(title))
	b.WriteString("\n\nName\n")
	b.WriteString(m.agentNameInput.View())
	b.WriteString("\n\nSystem prompt (ctrl+j for newline)\n")
	b.WriteString(m.agentPrompt.View())
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("[tab] switch field  [ctrl+s] save  [esc] cancel"))
	if m.agentError != "" {
		b.WriteString("\n" + errStyle.Render(m.agentError))
	}
	return b.String()
}

func (m *Model) viewChat() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(fmt.Sprintf("%s · %s", m.activeChat.Title, m.activeAgent.Name)))
	b.WriteString("\n\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	b.WriteString(m.chatStatusLine())
	b.WriteString("\n")

	if m.commandTimedOut {
		b.WriteString(errStyle.Render("Command is taking too long (>30s)."))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("[K]ill  [W]ait"))
	} else if m.commandRunning {
		terminalName := "unavailable"
		if entry := m.terminalByID(m.commandTerminalID); entry != nil {
			terminalName = entry.name
		}
		b.WriteString(dimStyle.Render("Command is running in " + terminalName + "; Enter sends input to the shell."))
		b.WriteString("\n" + m.textarea.View())
		b.WriteString("\n" + helpStyle.Render("[enter] send terminal input  [ctrl+j] newline  [ctrl+t] view terminal output"))
	} else if m.pendingToolCall != nil {
		b.WriteString(dimStyle.Render(fmt.Sprintf("Execute in %s: %s ?", m.activeTerminalName(), m.pendingCmd)))
		b.WriteString("\n")
		if m.activeChat.TrustExecution && isRememberableCommand(m.pendingCmd) {
			b.WriteString(helpStyle.Render("[enter/y] approve & remember exact command  [n]o"))
		} else if m.activeChat.TrustExecution {
			b.WriteString(helpStyle.Render("[enter/y] run once (cannot remember shell syntax)  [n]o"))
		} else {
			b.WriteString(helpStyle.Render("[enter/y] run once  [n]o"))
		}
	} else if m.fileToolRunning {
		b.WriteString(fmt.Sprintf("%s Working with file...", m.spinner.View()))
	} else if m.isStreaming {
		if m.finalAnswerRetry {
			thinkingLabel := "on"
			if !m.activeChat.Thinking {
				thinkingLabel = "off"
			}
			b.WriteString(fmt.Sprintf("%s Retrying with thinking %s...", m.spinner.View(), thinkingLabel))
			if m.finalAnswerTokens > maxResponseTokens {
				b.WriteString(" (same token budget)")
			}
		} else {
			b.WriteString(fmt.Sprintf("%s Streaming... [esc] to cancel", m.spinner.View()))
		}
	} else {
		b.WriteString(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Width(m.viewport.Width).Render(m.textarea.View()))
		mouseState := "off"
		if m.mouseOn {
			mouseState = "on"
		}
		b.WriteString("\n" + helpStyle.Render("[enter] send  [ctrl+j] newline  [ctrl+y] copy answer  [ctrl+b] copy code  [click block] copy  [pgup/pgdown] scroll  [f2] mouse:"+mouseState+"  [f3] thinking  [f4] trusted execution  [f5] tools  [ctrl+t] terminals  [esc] back"))
	}
	if m.copyStatus != "" {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓ "+m.copyStatus))
	}
	if m.contextNotice != "" {
		b.WriteString("\n" + helpStyle.Render(m.contextNotice))
	}
	if m.chatError != "" {
		b.WriteString("\n" + errStyle.Render(m.chatError))
	}
	if m.storageError != "" {
		b.WriteString("\n" + errStyle.Render(m.storageError))
	}
	if m.terminalNotice != "" {
		b.WriteString("\n" + errStyle.Render(m.terminalNotice))
	}
	return b.String()
}

func (m *Model) activeTerminalName() string {
	entry := m.activeTerminal()
	if entry == nil {
		return "unavailable"
	}
	name := entry.name
	if entry.starting {
		name += " (starting)"
	} else if entry.err != "" {
		name += " (unavailable)"
	}
	return name
}

func (m *Model) chatStatusLine() string {
	width := max(1, m.viewport.Width)
	thinkingEnabled := m.activeChat.Thinking
	thinkingBadge := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	if thinkingEnabled {
		thinkingBadge = thinkingBadge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42"))
	} else {
		thinkingBadge = thinkingBadge.Foreground(lipgloss.Color("15")).Background(lipgloss.Color("160"))
	}
	thinkingLabel := "THINKING OFF"
	if thinkingEnabled {
		thinkingLabel = "THINKING ON"
	} else if m.finalAnswerRetry {
		thinkingLabel = "THINKING OFF (RETRY)"
	}
	if m.finalAnswerRetry && m.activeChat.Thinking {
		thinkingLabel = "THINKING ON (RETRY)"
	}
	thinking := ansi.Wrap(thinkingBadge.Render(thinkingLabel), width, "")
	executionBadge := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	executionLabel := "TRUST EXEC OFF"
	if m.activeChat.TrustExecution {
		executionBadge = executionBadge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("214"))
		executionLabel = "TRUST EXEC ON"
	} else {
		executionBadge = executionBadge.Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	}
	execution := ansi.Wrap(executionBadge.Render(executionLabel), width, "")
	toolsBadge := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	toolsLabel := "TOOLS OFF"
	if m.activeChat.ToolsEnabled {
		toolsBadge = toolsBadge.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42"))
		toolsLabel = "TOOLS ON"
	} else {
		toolsBadge = toolsBadge.Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	}
	tools := ansi.Wrap(toolsBadge.Render(toolsLabel), width, "")
	terminalBadge := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("24"))
	activeTerminal := ansi.Wrap(terminalBadge.Render("TERMINAL: "+m.activeTerminalName()), width, "")
	badges := []string{thinking, execution, tools, activeTerminal}
	totalWidth := 0
	for _, badge := range badges {
		totalWidth += lipgloss.Width(badge)
	}
	if totalWidth+2*(len(badges)-1) > width {
		return strings.Join(badges, "\n")
	}
	return strings.Join(badges, "  ")
}
