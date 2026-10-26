package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const terminalScrollback = 1 << 20 // 1 MiB per terminal

// An abandoned terminal (app killed, no reattach) must not hold a PTY and a
// shell forever. The app closes its terminal explicitly on stop; this only
// reaps shells that have had no stream and no input for the same 30-minute
// window the official terminal UI treats as idle.
const terminalIdleTTL = 30 * time.Minute

// terminal keeps the PTY and its output independent of any one SSE
// connection. A dropped mobile connection can reattach to the same shell and
// resume from a byte offset instead of losing the session.
type terminal struct {
	id      string
	session string

	mu       sync.Mutex
	ptmx     *os.File
	cmd      *exec.Cmd
	cols     int
	rows     int
	dead     bool
	streams  int
	lastUsed time.Time
	wake     chan struct{}
	outMu    sync.Mutex
	out      []byte // scrollback; outAt is the absolute offset of out[0]
	outAt    int64
	onExit   func()
}

type terminalManager struct {
	mu       sync.Mutex
	terms    map[string]*terminal
	mountNS  bool
	workRoot string
}

func newTerminalManager(mountNS bool, workRoot string) *terminalManager {
	m := &terminalManager{terms: map[string]*terminal{}, mountNS: mountNS, workRoot: workRoot}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for now := range ticker.C {
			m.reapIdle(now)
		}
	}()
	return m
}

// reapIdle closes terminals with no attached stream that have been idle past
// terminalIdleTTL. Attached streams are never reaped.
func (m *terminalManager) reapIdle(now time.Time) {
	m.mu.Lock()
	var idle []*terminal
	for _, t := range m.terms {
		t.mu.Lock()
		stale := t.streams == 0 && now.Sub(t.lastUsed) > terminalIdleTTL
		t.mu.Unlock()
		if stale {
			idle = append(idle, t)
		}
	}
	m.mu.Unlock()
	for _, t := range idle {
		t.close()
		m.remove(t.id)
	}
}

func (m *terminalManager) start(sessionID string, cols, rows int) (*terminal, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id")
	}
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 30
	}
	root := m.workRoot
	if root == "" {
		root = envOr("MINIS_SANDBOX_WORK_ROOT", "/workspace/sessions")
	}
	sb, err := NewSandbox(SandboxConfig{
		WorkRoot:       root,
		MountNamespace: m.mountNS,
		MaxOutput:      0,
	}, sessionID)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	home := sb.Workspace()
	if m.mountNS {
		cmd = exec.Command("unshare", "-m", "--propagation", "private",
			"/usr/local/bin/minis-session-exec", "/bin/bash", "-l")
		home = "/var/minis/workspace"
	} else {
		cmd = exec.Command("/bin/bash", "-l")
	}
	cmd.Dir = sb.Workspace()
	env := append(os.Environ(),
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME="+home,
		"PWD="+home,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"MINIS_SESSION_ID="+sessionID,
		"MINIS_WORKSPACE="+home,
	)
	if m.mountNS {
		env = append(env, "MINIS_SESSION_ROOT="+sb.root)
	}
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	t := &terminal{
		id:       newID("term"),
		session:  sessionID,
		ptmx:     ptmx,
		cmd:      cmd,
		cols:     cols,
		rows:     rows,
		wake:     make(chan struct{}, 1),
		lastUsed: time.Now(),
	}
	// A new terminal for the same session replaces an abandoned shell from a
	// previous app process instead of leaking it.
	m.mu.Lock()
	var old []*terminal
	for id, existing := range m.terms {
		if existing.session == sessionID {
			old = append(old, existing)
			delete(m.terms, id)
		}
	}
	m.terms[t.id] = t
	t.onExit = func() { m.remove(t.id) }
	m.mu.Unlock()
	for _, existing := range old {
		existing.close()
	}
	go t.readLoop()
	return t, nil
}

func (m *terminalManager) get(id string) *terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terms[id]
}

func (m *terminalManager) remove(id string) {
	m.mu.Lock()
	delete(m.terms, id)
	m.mu.Unlock()
}

func (t *terminal) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *terminal) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		t.mu.Lock()
		ptmx := t.ptmx
		t.mu.Unlock()
		if ptmx == nil {
			return
		}
		n, err := ptmx.Read(buf)
		if n > 0 {
			t.outMu.Lock()
			t.out = append(t.out, buf[:n]...)
			if len(t.out) > terminalScrollback {
				drop := len(t.out) - terminalScrollback
				t.out = append([]byte(nil), t.out[drop:]...)
				t.outAt += int64(drop)
			}
			t.outMu.Unlock()
			// touch takes t.mu: keep the same outMu -> mu order as snapshot.
			t.touch()
			t.signal()
		}
		if err != nil {
			t.mu.Lock()
			t.dead = true
			onExit := t.onExit
			t.mu.Unlock()
			t.close()
			if onExit != nil {
				onExit()
			}
			t.signal()
			return
		}
	}
}

func (t *terminal) close() {
	t.mu.Lock()
	ptmx := t.ptmx
	cmd := t.cmd
	t.ptmx = nil
	t.cmd = nil
	t.dead = true
	t.mu.Unlock()
	if ptmx != nil {
		_ = ptmx.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		go func() { _ = cmd.Wait() }()
	}
	t.signal()
}

func (t *terminal) touch() {
	t.mu.Lock()
	t.lastUsed = time.Now()
	t.mu.Unlock()
}

func (t *terminal) attach() {
	t.mu.Lock()
	t.streams++
	t.lastUsed = time.Now()
	t.mu.Unlock()
}

func (t *terminal) detach() {
	t.mu.Lock()
	if t.streams > 0 {
		t.streams--
	}
	t.lastUsed = time.Now()
	t.mu.Unlock()
}

func (t *terminal) write(data []byte) error {
	t.mu.Lock()
	ptmx := t.ptmx
	t.mu.Unlock()
	defer t.touch()
	if ptmx == nil {
		return fmt.Errorf("terminal closed")
	}
	_, err := ptmx.Write(data)
	return err
}

func (t *terminal) resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ptmx == nil {
		return fmt.Errorf("terminal closed")
	}
	t.cols, t.rows = cols, rows
	t.lastUsed = time.Now()
	return pty.Setsize(t.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (t *terminal) snapshot(after int64) (chunk []byte, start, next int64, closed bool, skipped int64) {
	t.outMu.Lock()
	defer t.outMu.Unlock()
	if after < t.outAt {
		skipped = t.outAt - after
		after = t.outAt
	}
	start = after
	idx := after - t.outAt
	if idx < 0 {
		idx = 0
	}
	if idx < int64(len(t.out)) {
		chunk = append([]byte(nil), t.out[idx:]...)
	}
	next = t.outAt + int64(len(t.out))
	t.mu.Lock()
	closed = t.dead
	t.mu.Unlock()
	return
}

func (a *sandboxAgent) streamTerminal(w http.ResponseWriter, r *http.Request, term *terminal, mgr *terminalManager, after int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	writeEvent := func(name string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	term.mu.Lock()
	cols, rows := term.cols, term.rows
	term.mu.Unlock()
	if !writeEvent("ready", map[string]any{"id": term.id, "session_id": term.session, "cols": cols, "rows": rows, "offset": after}) {
		return
	}
	term.attach()
	defer term.detach()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		chunk, start, next, closed, skipped := term.snapshot(after)
		if skipped > 0 {
			if !writeEvent("gap", map[string]any{"offset": start, "skipped": skipped}) {
				return
			}
		}
		if len(chunk) > 0 {
			// start is the actual first buffered byte; when the caller's offset
			// fell behind a trimmed scrollback, labeling the chunk with the
			// stale `after` would make the client's byte count drift.
			if !writeEvent("output", map[string]any{"offset": start, "data": base64.StdEncoding.EncodeToString(chunk)}) {
				return
			}
			after = next
			continue
		}
		if closed {
			term.close()
			mgr.remove(term.id)
			writeEvent("exit", map[string]any{"reason": "process exited"})
			return
		}
		select {
		case <-term.wake:
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *sandboxAgent) terminalHandler(mgr *terminalManager) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
			Cols      int    `json:"cols"`
			Rows      int    `json:"rows"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if req.SessionID == "" {
			req.SessionID = "terminal"
		}
		term, err := mgr.start(req.SessionID, req.Cols, req.Rows)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.streamTerminal(w, r, term, mgr, 0)
	}))
}

func (a *sandboxAgent) terminalAttach(mgr *terminalManager) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		term := mgr.get(r.PathValue("id"))
		if term == nil {
			writeError(w, http.StatusNotFound, "terminal not found")
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if after < 0 {
			after = 0
		}
		a.streamTerminal(w, r, term, mgr, after)
	}))
}

func (a *sandboxAgent) terminalInput(mgr *terminalManager) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		term := mgr.get(r.PathValue("id"))
		if term == nil {
			writeError(w, http.StatusNotFound, "terminal not found")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*1024*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := term.write(data); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (a *sandboxAgent) terminalResize(mgr *terminalManager) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		term := mgr.get(r.PathValue("id"))
		if term == nil {
			writeError(w, http.StatusNotFound, "terminal not found")
			return
		}
		var req struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := term.resize(req.Cols, req.Rows); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (a *sandboxAgent) terminalClose(mgr *terminalManager) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		term := mgr.get(r.PathValue("id"))
		if term != nil {
			term.close()
			mgr.remove(term.id)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
