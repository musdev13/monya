package agenttools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileReturnsRequestedLineRange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src", "main.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Execute(root, "read_file", `{"path":"src/main.go","start_line":2,"max_lines":1}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "2: second\n") || !strings.Contains(got, "start_line=3") {
		t.Fatalf("read_file output = %q", got)
	}
}

func TestFileToolsRejectPathsOutsideProjectRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"../secret.txt", outside} {
		if _, err := Execute(root, "read_file", `{"path":`+quoteJSON(path)+`}`); err == nil {
			t.Errorf("read_file accepted outside path %q", path)
		}
		if _, err := Execute(root, "create_file", `{"path":`+quoteJSON(path)+`,"content":"bad"}`); err == nil {
			t.Errorf("create_file accepted outside path %q", path)
		}
	}
}

func TestCreateFileCreatesNewFileButNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	args := `{"path":"new/nested/file.txt","content":"first"}`
	if _, err := Execute(root, "create_file", args); err != nil {
		t.Fatalf("create_file: %v", err)
	}
	path := filepath.Join(root, "new", "nested", "file.txt")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("file content = %q", got)
	}

	if _, err := Execute(root, "create_file", `{"path":"new/nested/file.txt","content":"second"}`); err == nil {
		t.Fatal("create_file overwrote an existing file")
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after rejected overwrite: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("existing file changed to %q", got)
	}
}

func TestFileToolsCannotFollowSymlinkOutsideProjectRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "external")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	if _, err := Execute(root, "read_file", `{"path":"external/secret.txt"}`); err == nil {
		t.Fatal("read_file followed a symlink outside the project root")
	}
	if _, err := Execute(root, "create_file", `{"path":"external/new.txt","content":"bad"}`); err == nil {
		t.Fatal("create_file followed a symlink outside the project root")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created: stat error = %v", err)
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
