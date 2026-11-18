package agenttools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"monya/internal/api"
)

const (
	maxFileBytes   = 1 << 20
	maxOutputBytes = 16 << 10
	defaultLines   = 120
	maxLines       = 500
)

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	MaxLines  int    `json:"max_lines"`
}

type listDirectoryArgs struct {
	Path string `json:"path"`
}

type createFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func Definitions() []api.Tool {
	return []api.Tool{
		{
			Type: "function",
			Function: api.FunctionDef{
				Name:        "list_directory",
				Description: "Показать файлы и папки внутри каталога проекта Monya. Если path не указан, показать корневой каталог проекта. Используй для просмотра содержимого текущей папки; инструмент не требует подтверждения.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Путь каталога относительно корня проекта; по умолчанию корень проекта"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: api.FunctionDef{
				Name:        "read_file",
				Description: "Прочитать текстовый файл внутри каталога, из которого запущена Monya. Укажи относительный path; можно читать фрагмент через start_line и max_lines. Инструмент не требует подтверждения.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Путь относительно каталога запуска Monya"},
						"start_line": {"type": "integer", "minimum": 1, "description": "Первая строка (по умолчанию 1)"},
						"max_lines": {"type": "integer", "minimum": 1, "maximum": 500, "description": "Максимум строк (по умолчанию 120)"}
					},
					"required": ["path"]
				}`),
			},
		},
		{
			Type: "function",
			Function: api.FunctionDef{
				Name:        "create_file",
				Description: "Создать новый файл внутри каталога, из которого запущена Monya. Укажи относительный path и полный content. Родительские каталоги создаются автоматически; существующие файлы не перезаписываются. Инструмент не требует подтверждения.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Путь нового файла относительно каталога запуска Monya"},
						"content": {"type": "string", "description": "Полное содержимое нового файла"}
					},
					"required": ["path", "content"]
				}`),
			},
		},
	}
}

func IsFileTool(name string) bool {
	return name == "list_directory" || name == "read_file" || name == "create_file"
}

func Execute(rootPath, name, arguments string) (string, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("open project root: %w", err)
	}
	result, toolErr := execute(root, name, arguments)
	closeErr := root.Close()
	if toolErr != nil || closeErr != nil {
		return result, errors.Join(toolErr, closeErr)
	}
	return result, nil
}

func execute(root *os.Root, name, arguments string) (string, error) {
	switch name {
	case "list_directory":
		var args listDirectoryArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("invalid list_directory arguments: %w", err)
		}
		return listDirectory(root, args)
	case "read_file":
		var args readFileArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_file arguments: %w", err)
		}
		return readFile(root, args)
	case "create_file":
		var args createFileArgs
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("invalid create_file arguments: %w", err)
		}
		return createFile(root, args)
	default:
		return "", fmt.Errorf("unknown file tool %q", name)
	}
}

func listDirectory(root *os.Root, args listDirectoryArgs) (string, error) {
	path, err := cleanDirectoryPath(args.Path)
	if err != nil {
		return "", err
	}
	dir, err := root.Open(path)
	if err != nil {
		return "", fmt.Errorf("open directory %q: %w", path, err)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil || closeErr != nil {
		return "", errors.Join(
			wrapIfError(readErr, fmt.Sprintf("read directory %q", path)),
			wrapIfError(closeErr, fmt.Sprintf("close directory %q", path)),
		)
	}
	if len(entries) == 0 {
		return "Directory is empty.", nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var output strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		if output.Len()+len(name)+1 > maxOutputBytes {
			output.WriteString("[directory listing truncated]\n")
			break
		}
		output.WriteString(name)
		output.WriteByte('\n')
	}
	return output.String(), nil
}

func readFile(root *os.Root, args readFileArgs) (string, error) {
	path, err := cleanPath(args.Path)
	if err != nil {
		return "", err
	}
	startLine := args.StartLine
	if startLine == 0 {
		startLine = 1
	}
	if startLine < 1 {
		return "", fmt.Errorf("start_line must be at least 1")
	}
	lineLimit := args.MaxLines
	if lineLimit == 0 {
		lineLimit = defaultLines
	}
	if lineLimit < 1 || lineLimit > maxLines {
		return "", fmt.Errorf("max_lines must be between 1 and %d", maxLines)
	}

	info, err := root.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() > maxFileBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte read limit", path, maxFileBytes)
	}
	file, err := root.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", errors.Join(readErr, closeErr)
	}
	if len(data) > maxFileBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte read limit", path, maxFileBytes)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%q is not a UTF-8 text file", path)
	}
	if len(data) == 0 {
		return fmt.Sprintf("%s (empty file)", path), nil
	}

	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if startLine > len(lines) {
		return "", fmt.Errorf("start_line %d exceeds the %d-line file", startLine, len(lines))
	}

	endLine := min(len(lines), startLine+lineLimit-1)
	var output strings.Builder
	for line := startLine; line <= endLine; line++ {
		content := strings.TrimSuffix(lines[line-1], "\r")
		formatted := fmt.Sprintf("%d: %s\n", line, content)
		if output.Len()+len(formatted)+128 > maxOutputBytes {
			if output.Len() == 0 {
				return "", fmt.Errorf("line %d exceeds the %d-byte tool output limit", line, maxOutputBytes)
			}
			output.WriteString(fmt.Sprintf("[output truncated; continue with start_line=%d]\n", line))
			return output.String(), nil
		}
		output.WriteString(formatted)
	}
	if endLine < len(lines) {
		output.WriteString(fmt.Sprintf("[more lines available; continue with start_line=%d]\n", endLine+1))
	}
	return output.String(), nil
}

func createFile(root *os.Root, args createFileArgs) (string, error) {
	path, err := cleanPath(args.Path)
	if err != nil {
		return "", err
	}
	if len(args.Content) > maxFileBytes {
		return "", fmt.Errorf("content exceeds the %d-byte file limit", maxFileBytes)
	}
	if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create parent directories for %q: %w", path, err)
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %q: %w", path, err)
	}
	n, writeErr := io.WriteString(file, args.Content)
	if writeErr == nil && n != len(args.Content) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		removeErr := root.Remove(path)
		return "", errors.Join(
			wrapIfError(writeErr, "write new file"),
			wrapIfError(closeErr, "close new file"),
			wrapIfError(removeErr, "remove incomplete file"),
		)
	}
	return fmt.Sprintf("Created %s (%d bytes)", path, n), nil
}

func cleanPath(name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("path must be relative to the project root and must not escape it")
	}
	path := filepath.Clean(name)
	if path == "." || path == "" {
		return "", fmt.Errorf("path must name a file")
	}
	return path, nil
}

func cleanDirectoryPath(name string) (string, error) {
	if name == "" || name == "." {
		return ".", nil
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("path must be relative to the project root and must not escape it")
	}
	return filepath.Clean(name), nil
}

func wrapIfError(err error, message string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}
