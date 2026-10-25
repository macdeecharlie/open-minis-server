package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Sandbox gives every session a persistent workspace while keeping file tool
// paths inside it. It deliberately maps the Android client's virtual paths
// (/var/minis/workspace, /var/minis/attachments, ...) onto the server
// workspace so the existing system prompt and skills keep working unchanged.
// ToolSandbox is the execution boundary used by the agent loop. It is
// implemented by the in-process workspace (local mode) and by the dedicated
// full-Linux sandbox container client (agent mode).
type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

type ToolSandbox interface {
	Workspace() string
	RunShell(ctx context.Context, command string, timeout time.Duration, delay time.Duration, emit func(string)) (ShellResult, error)
	ReadFile(p string, offset int, lines int, maxLength int) (string, error)
	WriteFile(p, content string, appendMode bool, createDirs bool) error
	EditFile(p, oldString, newString string, replaceAll bool) (int, error)
	ListDir(p string) ([]FileEntry, error)
	ReadRaw(p string) ([]byte, string, error)
	Upload(p string, data []byte) error
}

type Sandbox struct {
	cfg         SandboxConfig
	sessionID   string
	root        string
	workspace   string
	attachments string
	uid         int
	gid         int
}

func NewSandbox(cfg SandboxConfig, sessionID string) (*Sandbox, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, errors.New("invalid session id")
	}
	root := filepath.Join(cfg.WorkRoot, sessionID)
	s := &Sandbox{
		cfg:         cfg,
		sessionID:   sessionID,
		root:        root,
		workspace:   filepath.Join(root, "workspace"),
		attachments: filepath.Join(root, "attachments"),
		uid:         -1,
		gid:         -1,
	}
	for _, d := range []string{s.root, s.workspace, s.attachments, filepath.Join(root, "offloads"), filepath.Join(root, "browser")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
	}
	if os.Geteuid() == 0 && cfg.ShellUser != "" {
		if u, err := user.Lookup(cfg.ShellUser); err == nil {
			if uid, e1 := strconv.Atoi(u.Uid); e1 == nil {
				if gid, e2 := strconv.Atoi(u.Gid); e2 == nil {
					s.uid, s.gid = uid, gid
					// The first run owns the tree; later runs only need this if
					// something was created by root (or the data was restored
					// with different ownership). Avoid a full walk per turn.
					alreadyOwned := false
					if st, err := os.Stat(root); err == nil {
						if sys, ok := st.Sys().(*syscall.Stat_t); ok {
							alreadyOwned = int(sys.Uid) == uid && int(sys.Gid) == gid
						}
					}
					if !alreadyOwned {
						_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
							if err == nil {
								_ = os.Chown(path, uid, gid)
							}
							return nil
						})
						_ = os.Chown(root, uid, gid)
					}
				}
			}
		}
	}
	return s, nil
}

func (s *Sandbox) Workspace() string { return s.workspace }

// NewToolSandbox selects the configured execution boundary. Local mode keeps
// the in-process workspace for tests and development; agent mode talks to the
// dedicated full-distro container described by sandbox.agent_url.
func NewToolSandbox(cfg SandboxConfig, sessionID string) (ToolSandbox, error) {
	if cfg.Mode == "agent" {
		return NewRemoteSandbox(cfg, sessionID)
	}
	return NewSandbox(cfg, sessionID)
}

// virtualPath maps the client-visible Linux paths onto this session's
// workspace. Anything outside the virtual roots is confined to the workspace.
func (s *Sandbox) virtualPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is empty")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains NUL")
	}
	var mapped string
	switch {
	case p == "/var/minis/workspace" || strings.HasPrefix(p, "/var/minis/workspace/"):
		mapped = filepath.Join(s.workspace, strings.TrimPrefix(p, "/var/minis/workspace"))
	case p == "/var/minis/attachments" || strings.HasPrefix(p, "/var/minis/attachments/"):
		mapped = filepath.Join(s.attachments, strings.TrimPrefix(p, "/var/minis/attachments"))
	case p == "/var/minis/offloads" || strings.HasPrefix(p, "/var/minis/offloads/"):
		mapped = filepath.Join(s.root, "offloads", strings.TrimPrefix(p, "/var/minis/offloads"))
	case p == "/var/minis/browser" || strings.HasPrefix(p, "/var/minis/browser/"):
		mapped = filepath.Join(s.root, "browser", strings.TrimPrefix(p, "/var/minis/browser"))
	case p == "/workspace" || strings.HasPrefix(p, "/workspace/"):
		mapped = filepath.Join(s.workspace, strings.TrimPrefix(p, "/workspace"))
	case filepath.IsAbs(p):
		// A real path is accepted only when it already points inside this
		// session. This lets the model use paths it just saw in shell output
		// without turning file_read into an arbitrary host-file reader.
		mapped = filepath.Clean(p)
	default:
		mapped = filepath.Join(s.workspace, p)
	}
	mapped = filepath.Clean(mapped)
	if err := s.assertWithin(mapped); err != nil {
		return "", fmt.Errorf("path escapes the session workspace: %s", p)
	}
	// A symlink inside the workspace is allowed only if its TARGET is also
	// inside the workspace. Without this check `ln -s /etc/passwd x` followed
	// by file_read("x") would read the host file.
	if resolved, err := filepath.EvalSymlinks(mapped); err == nil {
		if err := s.assertWithin(resolved); err != nil {
			return "", fmt.Errorf("path escapes the session workspace through a symlink: %s", p)
		}
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// The target does not exist yet (write). Resolve the parent so a symlinked
	// directory cannot redirect the write either.
	parent := filepath.Dir(mapped)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		if err := s.assertWithin(resolvedParent); err != nil {
			return "", fmt.Errorf("path escapes the session workspace through a symlink: %s", p)
		}
		mapped = filepath.Join(resolvedParent, filepath.Base(mapped))
	}
	return mapped, nil
}

func (s *Sandbox) assertWithin(path string) error {
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("outside workspace")
	}
	return nil
}

func (s *Sandbox) rewriteCommand(cmd string) string {
	replacer := strings.NewReplacer(
		"/var/minis/workspace", s.workspace,
		"/var/minis/attachments", s.attachments,
		"/var/minis/offloads", filepath.Join(s.root, "offloads"),
		"/var/minis/browser", filepath.Join(s.root, "browser"),
		"/workspace", s.workspace,
	)
	return replacer.Replace(cmd)
}

func (s *Sandbox) own(path string) {
	if s.uid >= 0 {
		_ = os.Chown(path, s.uid, s.gid)
	}
}

type ShellResult struct {
	Output   string
	ExitCode int
	TimedOut bool
}

type lineSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	maxLine int
	onLine  func(string)
}

func (w *lineSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			line := strings.TrimSuffix(w.buf.String(), "\r")
			w.buf.Reset()
			if w.onLine != nil && line != "" {
				w.onLine(line)
			}
			continue
		}
		if w.maxLine > 0 && w.buf.Len() >= w.maxLine {
			line := w.buf.String() + " … (line truncated)"
			w.buf.Reset()
			if w.onLine != nil {
				w.onLine(line)
			}
			continue
		}
		w.buf.WriteByte(b)
	}
	return len(p), nil
}

func (w *lineSink) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		line := strings.TrimSuffix(w.buf.String(), "\r")
		w.buf.Reset()
		if w.onLine != nil && line != "" {
			w.onLine(line)
		}
	}
}

func (s *Sandbox) RunShell(ctx context.Context, command string, timeout time.Duration, delay time.Duration, emit func(string)) (ShellResult, error) {
	if strings.TrimSpace(command) == "" {
		return ShellResult{}, errors.New("command is required")
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ShellResult{ExitCode: -1}, ctx.Err()
		case <-time.After(delay):
		}
	}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", s.rewriteCommand(command))
	cmd.Dir = s.workspace
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + s.workspace,
		"PWD=" + s.workspace,
		"MINIS_WORKSPACE=" + s.workspace,
		"MINIS_SESSION_ID=" + s.sessionID,
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if s.uid >= 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.uid), Gid: uint32(s.gid)}
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	var output bytes.Buffer
	var outputMu sync.Mutex
	maxOut := s.cfg.MaxOutput
	truncated := false
	emitTruncated := false
	noteSent := false
	sink := &lineSink{maxLine: 256 * 1024}
	sink.onLine = func(line string) {
		show := false
		emitNote := false
		outputMu.Lock()
		if maxOut > 0 && output.Len() >= maxOut {
			if !truncated {
				truncated = true
				output.WriteString("\n… output truncated by server …\n")
			}
			if !noteSent {
				noteSent = true
				emitNote = true
			}
			emitTruncated = true
		} else {
			output.WriteString(line)
			output.WriteByte('\n')
			show = !emitTruncated
		}
		outputMu.Unlock()
		if emit == nil {
			return
		}
		if show {
			emit(line)
		} else if emitNote {
			emit("… output truncated by server …")
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ShellResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ShellResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return ShellResult{}, err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(sink, stdout) }()
	go func() { defer wg.Done(); _, _ = io.Copy(sink, stderr) }()
	err = cmd.Wait()
	wg.Wait()
	sink.flush()

	res := ShellResult{Output: output.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		res.TimedOut = true
		res.ExitCode = -1
		if res.Output != "" {
			res.Output += "\n"
		}
		res.Output += fmt.Sprintf("[command timed out after %s]", timeout)
	}
	return res, nil
}

func (s *Sandbox) ReadFile(p string, offset int, lines int, maxLength int) (string, error) {
	path, err := s.virtualPath(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("file not found: %s", p)
		}
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", p)
	}
	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return fmt.Sprintf("[%s | %d bytes | binary file — cannot display contents]", p, st.Size()), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if maxLength <= 0 || maxLength > 80_000 {
		maxLength = 80_000
	}
	if offset < 1 {
		offset = 1
	}
	var selected []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNo := 0
	total := 0
	for sc.Scan() {
		total++
		lineNo++
		if lineNo < offset {
			continue
		}
		if lines > 0 && len(selected) >= lines {
			continue
		}
		selected = append(selected, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	content := strings.Join(selected, "\n")
	truncated := false
	if len(content) > maxLength {
		content = content[:maxLength]
		truncated = true
	}
	end := offset + len(selected) - 1
	if len(selected) == 0 {
		end = offset - 1
	}
	header := fmt.Sprintf("[%s | %d bytes | %d lines | showing %d-%d of %d", p, st.Size(), total, offset, end, total)
	if truncated {
		header += fmt.Sprintf(" | truncated at %d chars", maxLength)
	}
	header += "]"
	return header + "\n" + content, nil
}

func (s *Sandbox) WriteFile(p, content string, appendMode bool, createDirs bool) error {
	path, err := s.virtualPath(p)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.own(path)
	s.own(dir)
	return nil
}

func (s *Sandbox) EditFile(p, oldString, newString string, replaceAll bool) (int, error) {
	path, err := s.virtualPath(p)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("file not found: %s", p)
		}
		return 0, err
	}
	if st.Size() > 8*1024*1024 {
		return 0, errors.New("file is larger than 8 MB; use the shell for edits")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	src := string(b)
	count := strings.Count(src, oldString)
	if count == 0 {
		return 0, fmt.Errorf("old_string not found in %s", p)
	}
	if count > 1 && !replaceAll {
		return count, fmt.Errorf("old_string matches %d locations; set replace_all=true or provide more context", count)
	}
	replaced := count
	if replaceAll {
		src = strings.ReplaceAll(src, oldString, newString)
	} else {
		src = strings.Replace(src, oldString, newString, 1)
		replaced = 1
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return 0, err
	}
	s.own(path)
	return replaced, nil
}

func (s *Sandbox) ListDir(p string) ([]FileEntry, error) {
	path, err := s.virtualPath(p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileEntry{
			Name:     e.Name(),
			Path:     filepath.Join(p, e.Name()),
			IsDir:    e.IsDir(),
			Size:     info.Size(),
			Modified: info.ModTime().UnixMilli(),
		})
	}
	return out, nil
}

func (s *Sandbox) ReadRaw(p string) ([]byte, string, error) {
	path, err := s.virtualPath(p)
	if err != nil {
		return nil, "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if st.IsDir() {
		return nil, "", fmt.Errorf("path is a directory: %s", p)
	}
	if st.Size() > 64*1024*1024 {
		return nil, "", fmt.Errorf("file is larger than the 64 MB preview limit")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	contentType := "application/octet-stream"
	if byExt := mime.TypeByExtension(filepath.Ext(path)); byExt != "" {
		contentType = byExt
	} else if detected := http.DetectContentType(b); detected != "" {
		contentType = detected
	}
	return b, contentType, nil
}

func (s *Sandbox) Upload(p string, data []byte) error {
	path, err := s.virtualPath(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	s.own(path)
	return nil
}

func (s *Sandbox) Validate() error {
	if s.workspace == "" {
		return errors.New("missing workspace")
	}
	return nil
}
