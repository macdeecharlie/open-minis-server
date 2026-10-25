package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Store is a deliberately small append-only store: one directory per session
// with meta.json and messages.jsonl. It has no external database dependency,
// starts in milliseconds and keeps the session index in memory. That is the
// right trade-off for a personal agent server; a future Web UI can still list
// and read every session.
type Store struct {
	dir  string
	mu   sync.Mutex
	meta map[string]*SessionMeta
	ids  map[string]map[string]bool
}

func NewStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	s := &Store{dir: dir, meta: map[string]*SessionMeta{}, ids: map[string]map[string]bool{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read sessions dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !sessionIDPattern.MatchString(e.Name()) {
			continue
		}
		m, err := s.readMeta(e.Name())
		if err != nil {
			return err
		}
		s.meta[e.Name()] = m
	}
	return nil
}

func (s *Store) sessionDir(id string) string { return filepath.Join(s.dir, id) }
func (s *Store) metaPath(id string) string   { return filepath.Join(s.sessionDir(id), "meta.json") }
func (s *Store) logPath(id string) string    { return filepath.Join(s.sessionDir(id), "messages.jsonl") }

func (s *Store) readMeta(id string) (*SessionMeta, error) {
	b, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &SessionMeta{ID: id}, nil
		}
		return nil, fmt.Errorf("read session meta %s: %w", id, err)
	}
	var m SessionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse session meta %s: %w", id, err)
	}
	m.ID = id
	return &m, nil
}

func (s *Store) writeMeta(m *SessionMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dir := s.sessionDir(m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.metaPath(m.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(m.ID))
}

func (s *Store) EnsureSession(id, model string) (*SessionMeta, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, errors.New("invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta[id]
	if m == nil {
		m = &SessionMeta{ID: id, CreatedAt: nowMillis(), UpdatedAt: nowMillis(), Model: model}
		if err := s.writeMeta(m); err != nil {
			return nil, err
		}
		s.meta[id] = m
	}
	if model != "" && m.Model == "" {
		m.Model = model
		m.UpdatedAt = nowMillis()
		if err := s.writeMeta(m); err != nil {
			return nil, err
		}
	}
	cp := *m
	return &cp, nil
}

// AppendMessage stores a message once. It returns false when an identical id
// is already present, which makes a retried request idempotent.
func (s *Store) AppendMessage(msg StoredMessage) (bool, error) {
	if !sessionIDPattern.MatchString(msg.SessionID) {
		return false, errors.New("invalid session id")
	}
	if msg.ID == "" {
		return false, errors.New("message id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[msg.SessionID] == nil {
		seen, err := s.loadIDsLocked(msg.SessionID)
		if err != nil {
			return false, err
		}
		s.ids[msg.SessionID] = seen
	}
	if s.ids[msg.SessionID][msg.ID] {
		return false, nil
	}
	dir := s.sessionDir(msg.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if msg.CreatedAt == 0 {
		msg.CreatedAt = nowMillis()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(s.logPath(msg.SessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	s.ids[msg.SessionID][msg.ID] = true

	m := s.meta[msg.SessionID]
	if m == nil {
		m = &SessionMeta{ID: msg.SessionID, CreatedAt: msg.CreatedAt}
	}
	m.MessageCount++
	m.UpdatedAt = msg.CreatedAt
	if m.Title == "" && msg.Role == "user" {
		if text := firstText(msg.Parts); text != "" {
			m.Title = truncateTitle(text, 60)
		}
	}
	if err := s.writeMeta(m); err != nil {
		return true, err
	}
	s.meta[msg.SessionID] = m
	return true, nil
}

func (s *Store) loadIDsLocked(id string) (map[string]bool, error) {
	out := map[string]bool{}
	f, err := os.Open(s.logPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m StoredMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("parse messages for %s: %w", id, err)
		}
		out[m.ID] = true
	}
	return out, sc.Err()
}

func (s *Store) Messages(id string) ([]StoredMessage, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, errors.New("invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.logPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []StoredMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m StoredMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("parse message in %s: %w", id, err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func (s *Store) ListSessions() []SessionMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionMeta, 0, len(s.meta))
	for _, m := range s.meta {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

func (s *Store) GetSession(id string) (*SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta[id]
	if m == nil {
		return nil, os.ErrNotExist
	}
	cp := *m
	return &cp, nil
}

// UpdateSessionTitle changes a session's displayed title without touching its
// transcript, matching minis-sessions-cli update --title.
func (s *Store) UpdateSessionTitle(id, title string) error {
	if !sessionIDPattern.MatchString(id) {
		return errors.New("invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta[id]
	if m == nil {
		return os.ErrNotExist
	}
	m.Title = title
	m.UpdatedAt = nowMillis()
	if err := s.writeMeta(m); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeleteSession(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return errors.New("invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.meta, id)
	delete(s.ids, id)
	return os.RemoveAll(s.sessionDir(id))
}

func firstText(parts []WirePart) string {
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			return p.Text
		}
	}
	return ""
}

func truncateTitle(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	// Avoid cutting a UTF-8 sequence in half.
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, nowMillis())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
