package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"chatclaw/internal/define"
	"chatclaw/internal/errs"
	"chatclaw/internal/openclaw/runtime"
	"chatclaw/internal/services/toolchain"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// managedCommands defines which commands are routed to managed tools.
// Maps command name (lowercase) to toolchain tool name.
var managedCommands = map[string]string{
	"openclaw": "openclaw",
	"npm":      "npm",
	"npx":      "npx",
	"codex":    "codex",
	"uv":       "uv",
	"bun":      "bun",
	"bunx":     "bunx",
	"node":     "node",
}

// Session represents a terminal session.
type Session struct {
	ID      string `json:"id"`
	WorkDir string `json:"workDir"`
}

// TerminalService provides terminal session management and command execution.
type TerminalService struct {
	app       *application.App
	sessions  map[string]*Session
	mu        sync.RWMutex
	toolchain *toolchain.ToolchainService
}

// NewTerminalService creates a new TerminalService.
func NewTerminalService(app *application.App, tc *toolchain.ToolchainService) *TerminalService {
	return &TerminalService{
		app:       app,
		sessions:  make(map[string]*Session),
		toolchain: tc,
	}
}

// GetDefaultWorkDir returns the default working directory for terminal sessions.
func (s *TerminalService) GetDefaultWorkDir() (string, error) {
	openclawDir, err := define.OpenClawDataRootDir()
	if err != nil {
		return "", err
	}
	return openclawDir, nil
}

// CreateSession creates a new terminal session and returns its ID.
func (s *TerminalService) CreateSession() (*Session, error) {
	workDir, err := s.GetDefaultWorkDir()
	if err != nil {
		return nil, err
	}

	session := &Session{
		ID:      fmt.Sprintf("term-%d", len(s.sessions)+1),
		WorkDir: workDir,
	}

	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()

	return session, nil
}

// GetSession retrieves a session by ID.
func (s *TerminalService) GetSession(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

// CloseSession removes a terminal session.
func (s *TerminalService) CloseSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// ChangeDirectory changes the working directory of a session.
// Supports:
//   - Absolute paths
//   - Relative paths
//   - ~ for home directory
//   - Shortcuts: "openclaw:" for openclaw data dir
func (s *TerminalService) ChangeDirectory(sessionID, path string) (string, error) {
	session := s.GetSession(sessionID)
	if session == nil {
		return "", errs.New("error.terminal_session_not_found")
	}

	var newDir string
	path = strings.TrimSpace(path)

	// Handle shortcuts
	switch path {
	case "":
		newDir = session.WorkDir
	case "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		newDir = home
	case "openclaw:", "openclaw":
		openclawDir, err := define.OpenClawDataRootDir()
		if err != nil {
			return "", err
		}
		newDir = openclawDir
	default:
		// Handle relative paths
		if !filepath.IsAbs(path) {
			newDir = filepath.Join(session.WorkDir, path)
		} else {
			newDir = path
		}
	}

	// Resolve the path
	absDir, err := filepath.Abs(newDir)
	if err != nil {
		return "", err
	}

	// Check if directory exists
	info, err := os.Stat(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errs.Newf("error.terminal_directory_not_found", map[string]any{"Path": path})
		}
		return "", err
	}
	if !info.IsDir() {
		return "", errs.Newf("error.terminal_not_a_directory", map[string]any{"Path": path})
	}

	session.WorkDir = absDir
	return absDir, nil
}

// BundledBinDir returns the bundled bin directory from the full installer layout.
func (s *TerminalService) BundledBinDir() string {
	return s.bundledBinDir()
}

// bundledBinDir returns the bundled bin directory from the full installer layout.
// It checks <exeDir>/build/windows/bin (Windows) or <exeDir>/build/<os>-<arch>/bin (macOS/Linux).
// Returns empty string if the directory does not exist or is empty.
func (s *TerminalService) bundledBinDir() string {
	execPath, err := os.Executable()
	if err != nil || strings.TrimSpace(execPath) == "" {
		return ""
	}
	execDir := filepath.Dir(execPath)

	var bundled string
	if runtime.GOOS == "windows" {
		bundled = filepath.Join(execDir, "build", "windows", "bin")
	} else if runtime.GOOS == "darwin" {
		bundled = filepath.Join(execDir, "..", "Resources", "build", runtime.GOOS+"-"+runtime.GOARCH, "bin")
		if _, err := os.Stat(bundled); os.IsNotExist(err) {
			bundled = filepath.Join(execDir, "build", runtime.GOOS+"-"+runtime.GOARCH, "bin")
		}
	} else {
		bundled = filepath.Join(execDir, "build", runtime.GOOS+"-"+runtime.GOARCH, "bin")
	}

	if info, err := os.Stat(bundled); err != nil || !info.IsDir() {
		return ""
	}
	entries, err := os.ReadDir(bundled)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return bundled
}

// resolveToolPath returns the path to a managed tool.
func (s *TerminalService) resolveToolPath(toolName string) (string, error) {
	// For openclaw, use the bundled runtime CLI
	if toolName == "openclaw" {
		bundle, err := openclawruntime.ResolveBundledRuntime()
		if err == nil && bundle != nil {
			return bundle.CLIPath, nil
		}
	}

	// Check bundled bin dir first
	bundledBinDir := s.bundledBinDir()
	if bundledBinDir != "" {
		binName := s.getBinaryName(toolName)
		path := filepath.Join(bundledBinDir, binName)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Check app data bin dir
	if s.toolchain != nil {
		binDir := s.toolchain.BinDir()
		if binDir != "" {
			binName := s.getBinaryName(toolName)
			path := filepath.Join(binDir, binName)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}

	// For node/npm/npx, try to find from bundled runtime
	if toolName == "node" || toolName == "npm" || toolName == "npx" {
		nodePath := s.findNodePath()
		if nodePath != "" {
			return nodePath, nil
		}
	}

	// Try to find from system PATH
	if fullPath, err := exec.LookPath(toolName); err == nil {
		return fullPath, nil
	}

	return "", errs.Newf("error.terminal_tool_not_found", map[string]any{"Tool": toolName})
}

// findNodePath tries to find node from bundled runtime.
func (s *TerminalService) findNodePath() string {
	// Try to find node from openclaw bundled runtime
	// This uses the same logic as openclaw runtime service
	// Look in common locations
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	paths := []string{
		filepath.Join(home, ".chatclaw", "openclaw", "runtime", "current", "tools", "node", "bin", "node"),
		filepath.Join(home, ".chatclaw", "openclaw", "runtime", "windows-x64", "current", "tools", "node", "bin", "node.exe"),
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// getBinaryName returns the actual binary name for a tool.
func (s *TerminalService) getBinaryName(toolName string) string {
	switch toolName {
	case "openclaw":
		if runtime.GOOS == "windows" {
			return "openclaw.cmd"
		}
		return "openclaw"
	case "npm":
		if runtime.GOOS == "windows" {
			return "npm.cmd"
		}
		return "npm"
	case "npx":
		if runtime.GOOS == "windows" {
			return "npx.cmd"
		}
		return "npx"
	case "bun", "bunx":
		if runtime.GOOS == "windows" {
			return toolName + ".exe"
		}
		return toolName
	case "codex":
		if runtime.GOOS == "windows" {
			return "codex.exe"
		}
		return "codex"
	default:
		return toolName
	}
}

// CommandResult represents the result of a command execution.
type CommandResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ExecuteCommand executes a command in a terminal session.
func (s *TerminalService) ExecuteCommand(sessionID, cmdLine string) (*CommandResult, error) {
	session := s.GetSession(sessionID)
	if session == nil {
		return nil, errs.New("error.terminal_session_not_found")
	}

	// Parse command line
	parts := parseCommandLine(cmdLine)
	if len(parts) == 0 {
		return &CommandResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
	}

	baseCmd := strings.ToLower(parts[0])

	// Handle built-in commands
	if result, ok := s.handleBuiltinCommand(session, baseCmd, parts[1:]); ok {
		return result, nil
	}

	// Check if it's a managed command
	if managedToolName, isManaged := managedCommands[baseCmd]; isManaged {
		return s.executeManagedCommand(session, managedToolName, parts[1:])
	}

	// Execute via system PATH
	return s.executeSystemCommand(session, parts[0], parts[1:])
}

// handleBuiltinCommand handles built-in terminal commands like cd, pwd, clear.
func (s *TerminalService) handleBuiltinCommand(session *Session, cmd string, args []string) (*CommandResult, bool) {
	switch cmd {
	case "cd":
		if len(args) == 0 {
			return &CommandResult{ExitCode: 0, Stdout: "", Stderr: ""}, true
		}
		_, err := s.ChangeDirectory(session.ID, args[0])
		if err != nil {
			return &CommandResult{ExitCode: 1, Stdout: "", Stderr: err.Error()}, true
		}
		return &CommandResult{ExitCode: 0, Stdout: "", Stderr: ""}, true

	case "pwd":
		return &CommandResult{ExitCode: 0, Stdout: session.WorkDir + "\n", Stderr: ""}, true

	case "clear", "cls":
		return &CommandResult{ExitCode: 0, Stdout: "", Stderr: ""}, true

	default:
		return nil, false
	}
}

// executeManagedCommand executes a command using the managed toolchain.
func (s *TerminalService) executeManagedCommand(session *Session, toolName string, args []string) (*CommandResult, error) {
	toolPath, err := s.resolveToolPath(toolName)
	if err != nil {
		return &CommandResult{ExitCode: 1, Stdout: "", Stderr: err.Error() + "\n"}, nil
	}

	return s.runCommandWithEnv(session.WorkDir, toolPath, toolName, args...)
}

// executeSystemCommand executes a command via system PATH.
func (s *TerminalService) executeSystemCommand(session *Session, cmd string, args []string) (*CommandResult, error) {
	// First, try to find command in PATH
	fullPath, err := exec.LookPath(cmd)
	if err == nil {
		// Found in PATH, execute directly
		return s.runCommand(session.WorkDir, fullPath, args...)
	}

	// Command not found in PATH - try running via shell
	// This handles shell builtins like 'ls' (via sh), 'dir', 'type', etc.
	return s.runCommandViaShell(session.WorkDir, cmd, args)
}

// runCommandViaShell executes a command through the system shell.
// This handles shell builtins and commands not in PATH.
func (s *TerminalService) runCommandViaShell(workDir, cmd string, args []string) (*CommandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var shell string
	var shellArgs []string
	if runtime.GOOS == "windows" {
		// Use PowerShell on Windows for better compatibility with commands like ls, cat
		// Set UTF-8 code page (65001) and encoding for proper display of Chinese characters
		shell = "powershell.exe"
		shellArgs = []string{"-NoProfile", "-Command", "chcp 65001 > $null; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "}
	} else {
		shell = "/bin/sh"
		shellArgs = []string{"-c"}
	}

	// Build full command line
	fullCmd := cmd
	for _, arg := range args {
		// Quote arguments with spaces
		if strings.Contains(arg, " ") || strings.Contains(arg, "\"") {
			if runtime.GOOS == "windows" {
				arg = "\"" + strings.ReplaceAll(arg, "\"", "`\"") + "\""
			} else {
				arg = "\"" + strings.ReplaceAll(arg, "\"", "\\\"") + "\""
			}
		}
		fullCmd += " " + arg
	}

	allArgs := append(shellArgs, fullCmd)
	cmdExec := exec.CommandContext(ctx, shell, allArgs...)
	cmdExec.Dir = workDir
	cmdExec.Env = os.Environ()
	cmdExec.Env = append(cmdExec.Env, "NO_COLOR=1")

	// Hide window on Windows
	hideWindow(cmdExec)

	output, err := cmdExec.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return &CommandResult{
		ExitCode: exitCode,
		Stdout:   string(output),
		Stderr:   "",
	}, nil
}

// runCommand runs a command and returns the result.
func (s *TerminalService) runCommand(workDir, executable string, args ...string) (*CommandResult, error) {
	return s.runCommandWithEnv(workDir, executable, "", args...)
}

// runCommandWithEnv runs a command with additional environment variables.
func (s *TerminalService) runCommandWithEnv(workDir, executable, toolName string, args ...string) (*CommandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	// Disable color output via environment variable (widely supported)
	cmd.Env = append(cmd.Env, "NO_COLOR=1")

	// Set openclaw-specific environment variables
	if toolName == "openclaw" {
		stateDir, err := define.OpenClawDataRootDir()
		if err == nil {
			cmd.Env = append(cmd.Env, "OPENCLAW_STATE_DIR="+stateDir)
		}
	}

	// Hide window on Windows
	hideWindow(cmd)

	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return &CommandResult{
		ExitCode: exitCode,
		Stdout:   string(output),
		Stderr:   "",
	}, nil
}

// GetToolStatus returns the status of managed tools.
func (s *TerminalService) GetToolStatus() []ToolStatus {
	if s.toolchain == nil {
		return nil
	}

	statuses := s.toolchain.GetAllToolStatus()
	result := make([]ToolStatus, 0, len(statuses))

	for _, st := range statuses {
		result = append(result, ToolStatus{
			Name:             st.Name,
			Installed:        st.Installed,
			InstalledVersion: st.InstalledVersion,
			BinPath:         st.BinPath,
		})
	}

	return result
}

// ToolStatus represents the status of a managed tool.
type ToolStatus struct {
	Name             string `json:"name"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version"`
	BinPath          string `json:"bin_path"`
}

// parseCommandLine splits a command line into parts, respecting quotes.
func parseCommandLine(line string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	quoteChar := byte(0)

	for i := 0; i < len(line); i++ {
		c := line[i]

		if !inQuote && (c == '"' || c == '\'') {
			inQuote = true
			quoteChar = c
			continue
		}

		if inQuote && c == quoteChar {
			inQuote = false
			continue
		}

		if !inQuote && c == ' ' {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			continue
		}

		current.WriteByte(c)
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// PathCompletionResult represents a path completion result.
type PathCompletionResult struct {
	Matches []string `json:"matches"`
	IsDir   bool     `json:"is_dir"`
}

// GetPathCompletion returns possible completions for a partial path.
func (s *TerminalService) GetPathCompletion(sessionID, partialPath string) *PathCompletionResult {
	session := s.GetSession(sessionID)
	workDir := ""
	if session != nil {
		workDir = session.WorkDir
	}
	if workDir == "" {
		workDir, _ = s.GetDefaultWorkDir()
	}

	var baseDir, prefix string
	if strings.HasPrefix(partialPath, "/") || strings.HasPrefix(partialPath, "\\") {
		// Absolute path
		baseDir = filepath.Dir(partialPath)
		prefix = filepath.Base(partialPath)
	} else if strings.Contains(partialPath, "/") || strings.Contains(partialPath, "\\") {
		// Relative path with directory
		baseDir = filepath.Join(workDir, filepath.Dir(partialPath))
		prefix = filepath.Base(partialPath)
	} else {
		// Just a filename in current directory
		baseDir = workDir
		prefix = partialPath
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return &PathCompletionResult{Matches: []string{}, IsDir: false}
	}

	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			suffix := ""
			if entry.IsDir() {
				suffix = string(filepath.Separator)
			}
			matches = append(matches, name+suffix)
		}
	}

	return &PathCompletionResult{
		Matches: matches,
		IsDir:   false,
	}
}
