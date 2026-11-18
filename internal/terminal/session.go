package terminal

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const shellLoop = `while IFS=: read -r request_id encoded <&3; do
	command=$(printf '%s' "$encoded" | base64 -d 2>/dev/null) || command=""
	if eval "$command"; then
		status=0
	else
		status=$?
	fi
	cwd=$(printf '%s' "$PWD" | base64 | tr -d '\n')
	printf '\n\036MONYA_END:%s:%s:%s\037\n' "$request_id" "$status" "$cwd"
done`

const maxBufferedOutput = 2 * 1024 * 1024

type Result struct {
	Output string
	Status int
	CWD    string
}

type Session struct {
	cmd        *exec.Cmd
	input      *os.File
	control    *os.File
	output     *os.File
	done       chan struct{}
	changed    chan struct{}
	mu         sync.Mutex
	runMu      sync.Mutex
	out        []byte
	readErr    error
	waitErr    error
	closed     bool
	cwd        string
	dropped    int
	marker     []byte
	liveOffset int
}

func Start() (*Session, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("terminal: get current directory: %w", err)
	}

	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("terminal: create input pipe: %w", err)
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		inputRead.Close()
		inputWrite.Close()
		return nil, fmt.Errorf("terminal: create command pipe: %w", err)
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		inputRead.Close()
		inputWrite.Close()
		controlRead.Close()
		controlWrite.Close()
		return nil, fmt.Errorf("terminal: create output pipe: %w", err)
	}

	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", shellLoop)
	cmd.Stdin = inputRead
	cmd.Stdout = outputWrite
	cmd.Stderr = outputWrite
	cmd.ExtraFiles = []*os.File{controlRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		inputRead.Close()
		inputWrite.Close()
		controlRead.Close()
		controlWrite.Close()
		outputRead.Close()
		outputWrite.Close()
		return nil, fmt.Errorf("terminal: start bash: %w", err)
	}
	inputRead.Close()
	controlRead.Close()
	outputWrite.Close()

	session := &Session{
		cmd:     cmd,
		input:   inputWrite,
		control: controlWrite,
		output:  outputRead,
		done:    make(chan struct{}),
		changed: make(chan struct{}, 1),
		cwd:     cwd,
	}
	go session.readOutput()
	go func() {
		err := cmd.Wait()
		session.mu.Lock()
		session.waitErr = err
		session.mu.Unlock()
		close(session.done)
		session.signalOutput()
	}()
	return session, nil
}

func (s *Session) readOutput() {
	chunk := make([]byte, 8192)
	for {
		n, err := s.output.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.out = append(s.out, chunk[:n]...)
			if len(s.out) > maxBufferedOutput {
				drop := len(s.out) - maxBufferedOutput
				s.out = append([]byte(nil), s.out[drop:]...)
				s.dropped += drop
				s.liveOffset = max(0, s.liveOffset-drop)
			}
			s.mu.Unlock()
			s.signalOutput()
		}
		if err != nil {
			s.mu.Lock()
			s.readErr = err
			s.mu.Unlock()
			s.signalOutput()
			return
		}
	}
}

func (s *Session) signalOutput() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Session) Run(command string) (Result, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Result{}, errors.New("terminal session is closed")
	}
	s.mu.Unlock()
	requestID, err := NewRequestID()
	if err != nil {
		return Result{}, err
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	marker := []byte("\x1eMONYA_END:" + requestID + ":")
	s.mu.Lock()
	s.marker = marker
	s.liveOffset = 0
	s.mu.Unlock()
	if _, err := fmt.Fprintf(s.control, "%s:%s\n", requestID, encoded); err != nil {
		return Result{}, fmt.Errorf("terminal: send command: %w", err)
	}

	for {
		s.mu.Lock()
		if start := bytes.Index(s.out, marker); start >= 0 {
			endRel := bytes.IndexByte(s.out[start+len(marker):], 0x1f)
			if endRel >= 0 {
				end := start + len(marker) + endRel
				fields := strings.SplitN(string(s.out[start+len(marker):end]), ":", 2)
				if len(fields) != 2 {
					s.mu.Unlock()
					return Result{}, errors.New("terminal: malformed command result")
				}
				var status int
				if _, err := fmt.Sscanf(fields[0], "%d", &status); err != nil {
					s.mu.Unlock()
					return Result{}, fmt.Errorf("terminal: parse command status: %w", err)
				}
				cwdBytes, err := base64.StdEncoding.DecodeString(fields[1])
				if err != nil {
					s.mu.Unlock()
					return Result{}, fmt.Errorf("terminal: decode working directory: %w", err)
				}
				output := append([]byte(nil), s.out[:start]...)
				delivered := min(s.liveOffset, len(output))
				dropped := s.dropped
				s.dropped = 0
				remainder := s.out[end+1:]
				if len(remainder) > 0 && remainder[0] == '\n' {
					remainder = remainder[1:]
				}
				s.out = append(s.out[:0], remainder...)
				s.marker = nil
				s.liveOffset = 0
				s.cwd = string(cwdBytes)
				s.mu.Unlock()

				output = output[delivered:]
				if dropped > 0 {
					output = append([]byte("[earlier command output omitted]\n"), output...)
				}
				output = bytes.TrimSuffix(output, []byte("\n"))
				output = bytes.TrimSuffix(output, []byte("\r"))
				return Result{Output: string(output), Status: status, CWD: string(cwdBytes)}, nil
			}
		}
		readErr := s.readErr
		waitErr := s.waitErr
		selectDone := s.done
		s.mu.Unlock()

		if readErr != nil || selectDone != nil && isDone(selectDone) {
			if waitErr != nil {
				return Result{}, fmt.Errorf("terminal: shell exited: %w", waitErr)
			}
			return Result{}, errors.New("terminal: shell exited before command completed")
		}
		<-s.changed
	}
}

func (s *Session) DrainOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	visibleEnd := len(s.out)
	if s.marker != nil {
		if markerStart := bytes.Index(s.out, s.marker); markerStart >= 0 {
			visibleEnd = markerStart
		} else if keep := len(s.marker) - 1; keep > 0 && visibleEnd > keep {
			visibleEnd -= keep
		}
	}
	if visibleEnd <= s.liveOffset {
		return ""
	}
	chunk := string(append([]byte(nil), s.out[s.liveOffset:visibleEnd]...))
	s.liveOffset = visibleEnd
	return chunk
}

func isDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (s *Session) SendInput(input string) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("terminal session is closed")
	}
	if _, err := s.input.Write([]byte(input)); err != nil {
		return fmt.Errorf("terminal: send input: %w", err)
	}
	return nil
}

func (s *Session) Interrupt() error {
	s.mu.Lock()
	closed := s.closed
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	if closed || isDone(s.done) {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminal: interrupt command: %w", err)
	}
	return nil
}

func (s *Session) Kill() error {
	s.mu.Lock()
	closed := s.closed
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	if closed || isDone(s.done) {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminal: kill command: %w", err)
	}
	return nil
}

func (s *Session) CWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pid := s.cmd.Process.Pid
	_ = s.input.Close()
	_ = s.control.Close()
	s.mu.Unlock()

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminal: stop shell: %w", err)
	}
	select {
	case <-s.done:
	case <-time.After(500 * time.Millisecond):
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("terminal: kill shell: %w", err)
		}
		<-s.done
	}
	_ = s.output.Close()
	return nil
}

func NewRequestID() (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("terminal: generate request ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
