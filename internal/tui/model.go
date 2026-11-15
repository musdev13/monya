package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"monya/internal/api"
	"monya/internal/config"
	"monya/internal/store"
)

type state int

const (
	stateSessions state = iota
	stateChat
	stateNewSession
)

type (
	sessionsMsg []api.Agent
	errMsg      error

	streamStartedMsg struct{ stream *api.Stream }
	chunkMsg         api.StreamChunk
	streamDoneMsg    struct{}
	streamErrMsg     struct{ err error }

	commandDoneMsg struct {
		output string
		err    error
	}
	commandTimeoutMsg struct{}
)

type sessionItem struct{ agent api.Agent }

func (i sessionItem) Title() string       { return i.agent.Agent }
func (i sessionItem) Description() string { return fmt.Sprintf("%d messages", i.agent.MessageCount) }
func (i sessionItem) FilterValue() string { return i.agent.Agent }

type Model struct {
	client *api.Client
	cfg    *config.Config

	state state
	err   error
	busy  bool

	// Session list
	sessions []api.Agent
	list     list.Model

	// New session
	textInput textinput.Model

	// Chat
	activeSession string
	viewport      viewport.Model
	textarea      textarea.Model
	history       []api.Message

	// Streaming state
	spinner          spinner.Model
	isStreaming      bool
	stream           *api.Stream
	cancelStream     context.CancelFunc
	currentReasoning string
	currentResponse  string
	currentToolCalls []api.ToolCall

	// Command execution state
	pendingToolCall *api.ToolCall
	pendingCmd      string
	commandCancel   context.CancelFunc
	commandTimedOut bool
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
	l.Title = "Monya — Sessions"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	l.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new session")),
			key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		}
	}

	ti := textinput.New()
	ti.Placeholder = "New session name..."
	ti.CharLimit = 64

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return &Model{
		client:    client,
		cfg:       cfg,
		state:     stateSessions,
		busy:      true,
		textarea:  ta,
		viewport:  vp,
		list:      l,
		textInput: ti,
		spinner:   sp,
	}
}

func fetchSessions(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := client.ListSessions(ctx)
		if err != nil {
			return errMsg(err)
		}
		return sessionsMsg(resp.Agents)
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
	return tea.Batch(fetchSessions(m.client), m.list.StartSpinner(), m.spinner.Tick)
}

func (m *Model) startStream() tea.Cmd {
	m.isStreaming = true
	m.currentReasoning = ""
	m.currentResponse = ""
	m.currentToolCalls = nil
	m.updateViewport()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelStream = cancel

	reqHistory := make([]api.Message, len(m.history))
	copy(reqHistory, m.history)
	for i := range reqHistory {
		reqHistory[i].ReasoningContent = "" // API не ожидает это поле в запросе
	}

	req := api.ChatRequest{
		Model:    m.cfg.DefaultModel,
		User:     m.activeSession,
		Messages: reqHistory,
		Tools: []api.Tool{{
			Type: "function",
			Function: api.FunctionDef{
				Name:        "run_bash",
				Description: "Выполнить bash-команду на машине пользователя и вернуть stdout+stderr.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"command": {"type": "string", "description": "Bash-команда для выполнения"}
					},
					"required": ["command"]
				}`),
			},
		}},
	}

	return func() tea.Msg {
		stream, err := m.client.ChatCompletionStream(ctx, req)
		if err != nil {
			cancel()
			return streamErrMsg{err: err}
		}
		return streamStartedMsg{stream: stream}
	}
}

func executeCommand(ctx context.Context, cmdStr string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.CommandContext(ctx, "bash", "-lc", cmdStr)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf

		err := cmd.Run()
		out := buf.String()
		if len(out) > 16384 {
			out = out[:16384] + "\n...[truncated]"
		}
		return commandDoneMsg{output: out, err: err}
	}
}

func renderMarkdown(content string) string {
	r, err := glamour.Render(content, "dark")
	if err != nil {
		return content
	}
	return r
}

func (m *Model) updateViewport() {
	var b strings.Builder

	for _, msg := range m.history {
		switch msg.Role {
		case "user":
			b.WriteString(userStyle.Render("User: " + msg.Content))
		case "assistant":
			if msg.ReasoningContent != "" {
				b.WriteString(reasoningStyle.Render("Thoughts:\n" + msg.ReasoningContent))
				b.WriteString("\n\n")
			}
			b.WriteString("Assistant:\n" + renderMarkdown(msg.Content))
		case "tool":
			b.WriteString(toolStyle.Render(fmt.Sprintf("Tool (%s): %s", msg.Name, msg.Content)))
		}
		b.WriteString("\n\n")
	}

	if m.isStreaming {
		if m.currentReasoning != "" {
			b.WriteString(dimStyle.Render(m.currentReasoning))
			b.WriteString("\n\n")
		}
		if m.currentResponse != "" {
			b.WriteString("Assistant:\n" + m.currentResponse)
		}
	}

	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = msg.Width - 2
		m.viewport.Height = msg.Height - 10
		m.textarea.SetWidth(msg.Width - 4)
		m.list.SetSize(msg.Width, msg.Height-4) // leave space for custom footer
		m.updateViewport()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.isStreaming && m.cancelStream != nil {
				m.cancelStream()
			}
			if m.commandCancel != nil {
				m.commandCancel()
			}
			return m, tea.Quit
		}

	case sessionsMsg:
		m.busy = false
		m.err = nil
		m.sessions = msg
		m.list.StopSpinner()

		var items []list.Item
		for _, s := range msg {
			items = append(items, sessionItem{agent: s})
		}
		cmds = append(cmds, m.list.SetItems(items))
		return m, tea.Batch(cmds...)

	case errMsg:
		m.busy = false
		m.err = msg
		m.list.StopSpinner()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case streamStartedMsg:
		m.stream = msg.stream
		return m, m.readNextChunk()
	}

	// Routing
	if m.state == stateSessions {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "q" && m.list.FilterState() != list.Filtering {
				return m, tea.Quit
			}
			if msg.String() == "r" && m.list.FilterState() != list.Filtering {
				m.busy = true
				m.err = nil
				m.list.StartSpinner()
				return m, fetchSessions(m.client)
			}
			if msg.String() == "n" && m.list.FilterState() != list.Filtering {
				m.state = stateNewSession
				m.textInput.SetValue("")
				m.textInput.Focus()
				return m, textinput.Blink
			}
			if msg.String() == "enter" && m.list.FilterState() != list.Filtering {
				if i, ok := m.list.SelectedItem().(sessionItem); ok {
					m.activeSession = i.agent.Agent
					m.state = stateChat

					hist, loadErr := store.LoadHistory(m.activeSession)
					if loadErr == nil && hist != nil {
						m.history = hist
					} else {
						m.history = []api.Message{}
					}

					m.isStreaming = false
					m.pendingToolCall = nil
					m.updateViewport()
					m.textarea.SetValue("")
					m.textarea.Focus()
					return m, textarea.Blink
				}
			}
		}

		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	if m.state == stateNewSession {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				m.state = stateSessions
				m.textInput.Blur()
				return m, nil
			case "enter":
				name := strings.TrimSpace(m.textInput.Value())
				if name != "" {
					m.activeSession = name
					m.state = stateChat
					m.history = []api.Message{}

					hist, loadErr := store.LoadHistory(m.activeSession)
					if loadErr == nil && hist != nil {
						m.history = hist
					}

					m.isStreaming = false
					m.pendingToolCall = nil
					m.updateViewport()
					m.textInput.Blur()
					m.textarea.SetValue("")
					m.textarea.Focus()
					return m, textarea.Blink
				}
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)
		return m, cmd
	}

	if m.state == stateChat {
		switch msg := msg.(type) {
		case tea.MouseMsg:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd

		case tea.KeyMsg:
			if m.commandTimedOut {
				switch msg.String() {
				case "k", "K":
					if m.commandCancel != nil {
						m.commandCancel()
						m.commandCancel = nil
					}
					m.commandTimedOut = false
					m.history = append(m.history, api.Message{
						Role:    "tool",
						Content: "Command killed by user after 30s timeout.",
					})
					_ = store.SaveHistory(m.activeSession, m.history)
					m.updateViewport()
					return m, m.startStream()
				case "w", "W":
					m.commandTimedOut = false
					return m, nil
				}
				return m, nil
			}

			if m.pendingToolCall != nil {
				switch msg.String() {
				case "y", "Y", "enter":
					cmdStr := m.pendingCmd
					tcID := m.pendingToolCall.ID

					m.history = append(m.history, api.Message{
						Role:       "tool",
						ToolCallID: tcID,
						Name:       "run_bash",
						Content:    "Executing...",
					})
					m.pendingToolCall = nil
					m.commandTimedOut = false

					ctx, cancel := context.WithCancel(context.Background())
					m.commandCancel = cancel

					execCmd := executeCommand(ctx, cmdStr)
					timerCmd := tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
						return commandTimeoutMsg{}
					})
					m.updateViewport()
					return m, tea.Batch(execCmd, timerCmd)
				case "n", "N", "esc":
					tcID := m.pendingToolCall.ID
					m.history = append(m.history, api.Message{
						Role:       "tool",
						ToolCallID: tcID,
						Name:       "run_bash",
						Content:    "User denied execution.",
					})
					m.pendingToolCall = nil
					return m, m.startStream()
				}
				return m, nil
			}

			switch msg.String() {
			case "esc":
				if m.isStreaming {
					if m.cancelStream != nil {
						m.cancelStream()
					}
					m.isStreaming = false
					m.updateViewport()
					return m, nil
				}
				m.state = stateSessions
				m.textarea.Blur()

				// Обновляем список сессий при выходе
				m.busy = true
				m.err = nil
				m.list.StartSpinner()
				return m, fetchSessions(m.client)
			case "enter":
				text := strings.TrimSpace(m.textarea.Value())
				if text != "" && !m.isStreaming {
					m.history = append(m.history, api.Message{Role: "user", Content: text})
					_ = store.SaveHistory(m.activeSession, m.history)
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
				if d.ReasoningContent != "" {
					m.currentReasoning += d.ReasoningContent
				}
				if d.Content != "" {
					m.currentResponse += d.Content
				}
				if len(d.ToolCalls) > 0 {
					m.currentToolCalls = append(m.currentToolCalls, d.ToolCalls...)
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

			if len(m.currentToolCalls) > 0 {
				m.history = append(m.history, api.Message{
					Role:             "assistant",
					Content:          m.currentResponse,
					ReasoningContent: m.currentReasoning,
					ToolCalls:        m.currentToolCalls,
				})
				_ = store.SaveHistory(m.activeSession, m.history)

				tc := m.currentToolCalls[0]
				m.pendingToolCall = &tc
				m.currentReasoning = ""
				m.currentResponse = ""
				m.currentToolCalls = nil

				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					args.Command = "echo 'Failed to parse command JSON'"
				}
				m.pendingCmd = args.Command
				m.updateViewport()
			} else {
				if m.currentResponse != "" || m.currentReasoning != "" {
					m.history = append(m.history, api.Message{
						Role:             "assistant",
						Content:          m.currentResponse,
						ReasoningContent: m.currentReasoning,
					})
				}
				_ = store.SaveHistory(m.activeSession, m.history)
				m.currentReasoning = ""
				m.currentResponse = ""
				m.updateViewport()
			}
			return m, nil

		case streamErrMsg:
			m.isStreaming = false
			m.history = append(m.history, api.Message{Role: "assistant", Content: fmt.Sprintf("Error: %v", msg.err)})
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

		case commandTimeoutMsg:
			if m.commandCancel != nil {
				m.commandTimedOut = true
			}
			return m, nil

		case commandDoneMsg:
			content := msg.output
			if msg.err != nil {
				content += fmt.Sprintf("\n[exit error: %v]", msg.err)
			}

			if len(m.history) > 0 && m.history[len(m.history)-1].Role == "tool" {
				m.history[len(m.history)-1].Content = content
			}
			_ = store.SaveHistory(m.activeSession, m.history)
			m.updateViewport()

			m.commandTimedOut = false
			if m.commandCancel != nil {
				m.commandCancel()
				m.commandCancel = nil
			}

			return m, m.startStream()
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
	case stateSessions:
		if m.err != nil {
			return fmt.Sprintf("\n  %s\n\n  %s\n", errStyle.Render("Error: "+m.err.Error()), helpStyle.Render("[r]etry  [q]uit"))
		}
		footer := helpStyle.Render("[↑/↓] navigate  [/] search  [enter] select  [n] new session  [r] refresh  [q] quit")
		return "\n" + m.list.View() + "\n" + footer
	case stateChat:
		return m.viewChat()
	case stateNewSession:
		return m.viewNewSession()
	default:
		return "Unknown state\n"
	}
}

func (m *Model) viewNewSession() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("Create New Session"))
	b.WriteString("\n\n")
	b.WriteString(m.textInput.View())
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("[enter] create  [esc] cancel"))
	return b.String()
}

func (m *Model) viewChat() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(fmt.Sprintf("Chat: %s", m.activeSession)))
	b.WriteString("\n\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	if m.commandTimedOut {
		b.WriteString(errStyle.Render("Command is taking too long (>30s)."))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("[K]ill  [W]ait"))
	} else if m.pendingToolCall != nil {
		b.WriteString(dimStyle.Render(fmt.Sprintf("Execute: %s ?", m.pendingCmd)))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("[enter/y]es  [n]o"))
	} else if m.isStreaming {
		b.WriteString(fmt.Sprintf("%s Streaming... [esc] to cancel", m.spinner.View()))
	} else {
		b.WriteString(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Width(m.viewport.Width).Render(m.textarea.View()))
		b.WriteString("\n" + helpStyle.Render("[enter] send  [ctrl+j] newline  [pgup/pgdown] scroll  [esc] back"))
	}
	return b.String()
}
