package terminal

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPreservesShellStateBetweenCommands(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer session.Close()

	dir := filepath.Join(t.TempDir(), "working directory")
	first, err := session.Run("mkdir -p " + shellQuote(dir) + " && cd " + shellQuote(dir) + " && export MONYA_TEST_VALUE=kept")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Status != 0 || first.CWD != dir {
		t.Fatalf("first result = %+v, want cwd %q and status 0", first, dir)
	}

	second, err := session.Run("printf '%s|%s' \"$PWD\" \"$MONYA_TEST_VALUE\"")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Output != dir+"|kept" {
		t.Fatalf("second output = %q, want %q", second.Output, dir+"|kept")
	}
}

func TestRunAcceptsInputFromTerminal(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer session.Close()

	resultCh := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, runErr := session.Run("printf 'prompt>'; read answer; printf 'received:%s' \"$answer\"")
		resultCh <- struct {
			result Result
			err    error
		}{result: result, err: runErr}
	}()

	select {
	case <-time.After(100 * time.Millisecond):
		if output := session.DrainOutput(); !strings.Contains(output, "prompt>") {
			t.Fatalf("live output = %q, want prompt>", output)
		}
		if err := session.SendInput("hello\n"); err != nil {
			t.Fatalf("SendInput: %v", err)
		}
	case got := <-resultCh:
		t.Fatalf("command finished before input: result=%+v err=%v", got.result, got.err)
	}

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Run: %v", got.err)
		}
		if got.result.Output != "received:hello" {
			t.Fatalf("output = %q, want received:hello", got.result.Output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not finish after input")
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestCommandFailureReturnsExitStatus(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer session.Close()

	result, err := session.Run("printf 'failed'; false")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != 1 || result.Output != "failed" {
		t.Fatalf("result = %+v, want output failed and status 1", result)
	}
}

func TestKillStopsRunningShellCommand(t *testing.T) {
	session, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer session.Close()

	resultCh := make(chan error, 1)
	go func() {
		_, runErr := session.Run("sleep 30")
		resultCh <- runErr
	}()
	time.Sleep(100 * time.Millisecond)
	if err := session.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("Run succeeded after shell process was killed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after Kill")
	}
}
