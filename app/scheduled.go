package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ScheduledTask is the server-owned task model. The Android UI's local task
// model has more presentation fields; this is the executable contract: when,
// which prompt, which target/session/model, and what happened.
type ScheduledTask struct {
	ID          string         `json:"id"`
	Label       string         `json:"label"`
	Prompt      string         `json:"prompt"`
	Target      string         `json:"target"` // new | append
	SessionID   string         `json:"session_id,omitempty"`
	Model       string         `json:"model,omitempty"`
	Thinking    string         `json:"thinking,omitempty"`
	Enabled     bool           `json:"enabled"`
	Kind        string         `json:"kind"`           // once | interval | daily | weekdays | custom
	At          int64          `json:"at,omitempty"`   // once, epoch ms
	TimeOfDay   string         `json:"time,omitempty"` // HH:MM local
	Days        []int          `json:"days,omitempty"` // 0=Sunday
	Timezone    string         `json:"timezone,omitempty"`
	IntervalSec int64          `json:"interval_sec,omitempty"`
	MaxFires    int            `json:"max_fires,omitempty"`
	Fires       int            `json:"fires"`
	CreatedAt   int64          `json:"created_at"`
	UpdatedAt   int64          `json:"updated_at"`
	NextRunAt   int64          `json:"next_run_at"`
	LastRunAt   int64          `json:"last_run_at"`
	LastStatus  string         `json:"last_status,omitempty"`
	LastResult  string         `json:"last_result,omitempty"`
	RunHistory  []ScheduledRun `json:"run_history,omitempty"`
}

type ScheduledRun struct {
	At        int64  `json:"at"`
	SessionID string `json:"session_id,omitempty"`
	OK        bool   `json:"ok"`
	Preview   string `json:"preview,omitempty"`
}

type ScheduledStore struct {
	path string
	mu   sync.Mutex
	data map[string]*ScheduledTask
}

func NewScheduledStore(path string) (*ScheduledStore, error) {
	s := &ScheduledStore{path: path, data: map[string]*ScheduledTask{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var tasks []*ScheduledTask
	if err := json.Unmarshal(b, &tasks); err != nil {
		return nil, fmt.Errorf("parse scheduled tasks: %w", err)
	}
	for _, t := range tasks {
		if t != nil && t.ID != "" {
			s.data[t.ID] = t
		}
	}
	return s, nil
}

func (s *ScheduledStore) saveLocked() error {
	tasks := make([]*ScheduledTask, 0, len(s.data))
	for _, t := range s.data {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *ScheduledStore) List() []*ScheduledTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*ScheduledTask, 0, len(s.data))
	for _, t := range s.data {
		cp := *t
		cp.RunHistory = append([]ScheduledRun(nil), t.RunHistory...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *ScheduledStore) Get(id string) *ScheduledTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.data[id]; t != nil {
		cp := *t
		cp.RunHistory = append([]ScheduledRun(nil), t.RunHistory...)
		return &cp
	}
	return nil
}

func (s *ScheduledStore) Upsert(t *ScheduledTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[t.ID] = t
	return s.saveLocked()
}

func (s *ScheduledStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return s.saveLocked()
}

type Scheduler struct {
	cfg     *Config
	server  *Server
	store   *ScheduledStore
	wake    chan struct{}
	stop    context.CancelFunc
	mu      sync.Mutex
	running map[string]bool
}

func NewScheduler(cfg *Config, server *Server, store *ScheduledStore) *Scheduler {
	return &Scheduler{cfg: cfg, server: server, store: store, wake: make(chan struct{}, 1), running: map[string]bool{}}
}

func (s *Scheduler) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.stop = cancel
	go s.loop(ctx)
}

func (s *Scheduler) Stop() {
	if s.stop != nil {
		s.stop()
	}
}

func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	for {
		next := int64(0)
		for _, t := range s.store.List() {
			s.mu.Lock()
			running := s.running[t.ID]
			s.mu.Unlock()
			if running {
				continue
			}
			if t.Enabled && t.NextRunAt > 0 && (next == 0 || t.NextRunAt < next) {
				next = t.NextRunAt
			}
		}
		if next == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		delay := time.Until(time.UnixMilli(next))
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
			nowMs := time.Now().UnixMilli()
			for _, t := range s.store.List() {
				if t.Enabled && t.NextRunAt > 0 && t.NextRunAt <= nowMs {
					s.startTask(ctx, t)
				}
			}
		}
	}
}

func (s *Scheduler) startTask(ctx context.Context, t *ScheduledTask) {
	s.mu.Lock()
	if s.running[t.ID] {
		s.mu.Unlock()
		return
	}
	s.running[t.ID] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, t.ID)
			s.mu.Unlock()
			s.Wake()
		}()
		s.runTask(ctx, t, false)
	}()
}

func (s *Scheduler) RunNow(ctx context.Context, id string) error {
	t := s.store.Get(id)
	if t == nil {
		return os.ErrNotExist
	}
	s.runTask(ctx, t, true)
	return nil
}

func (s *Scheduler) runTask(ctx context.Context, t *ScheduledTask, manual bool) {
	start := time.Now()
	sessionID, text, err := s.server.runScheduled(ctx, t)
	run := ScheduledRun{At: start.UnixMilli(), SessionID: sessionID, OK: err == nil, Preview: truncate(text, 300)}
	if err != nil {
		run.Preview = truncate(err.Error(), 300)
		log.Printf("scheduled task %s failed: %v", t.ID, err)
	}
	updated := s.store.Get(t.ID)
	if updated == nil {
		return
	}
	updated.Fires++
	updated.LastRunAt = start.UnixMilli()
	updated.LastStatus = "ok"
	updated.LastResult = run.Preview
	if err != nil {
		updated.LastStatus = "error"
	}
	if updated.Kind == "once" {
		updated.Enabled = false
		updated.NextRunAt = 0
	}
	updated.RunHistory = append([]ScheduledRun{run}, updated.RunHistory...)
	if len(updated.RunHistory) > 50 {
		updated.RunHistory = updated.RunHistory[:50]
	}
	if !manual {
		updated.NextRunAt = nextRunAfter(updated, start)
		if updated.MaxFires > 0 && updated.Fires >= updated.MaxFires {
			updated.Enabled = false
			updated.NextRunAt = 0
		}
	}
	if err := s.store.Upsert(updated); err != nil {
		log.Printf("save scheduled task: %v", err)
	}
}

// runScheduled builds a headless agent run from the stored session history.
// It intentionally executes server tools only; device tools return a visible
// error because no paired app is guaranteed to be online.
func (s *Server) runScheduled(ctx context.Context, t *ScheduledTask) (string, string, error) {
	sid := t.SessionID
	if t.Target == "" || t.Target == "new" || sid == "" {
		prefix := strings.ReplaceAll(t.ID, "-", "")
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		if prefix == "" {
			prefix = "task"
		}
		sid = fmt.Sprintf("sched_%s_%d", prefix, time.Now().Unix())
	}
	if _, err := s.store.EnsureSession(sid, t.Model); err != nil {
		return sid, "", err
	}
	stored, err := s.store.Messages(sid)
	if err != nil {
		return sid, "", err
	}
	msgs := make([]WireMessage, 0, len(stored)+1)
	for _, m := range stored {
		msgs = append(msgs, WireMessage{Role: m.Role, Parts: m.Parts, ReasoningContent: m.ReasoningContent})
	}
	msgs = append(msgs, WireMessage{Role: "user", Parts: []WirePart{{Type: "text", Text: t.Prompt}}})
	req := RunRequest{
		RunID:         newID("schedrun"),
		SessionID:     sid,
		Model:         t.Model,
		System:        "",
		Messages:      msgs,
		Tools:         s.buildTools(nil),
		MaxTurns:      20,
		MaxTokens:     8192,
		Thinking:      t.Thinking,
		UserMessageID: newID("scheduser"),
	}
	run := newRun(ctx, req.RunID)
	run.Headless = true
	go s.runAgent(req, run)
	var finalText strings.Builder
	var runErr error
	for ev := range run.events {
		switch ev.Name {
		case "assistant_end":
			if m, ok := ev.Data.(map[string]any); ok {
				if parts, ok := m["parts"].([]WirePart); ok {
					for _, p := range parts {
						if p.Type == "text" {
							finalText.WriteString(p.Text)
						}
					}
				}
			}
		case "error":
			if m, ok := ev.Data.(map[string]any); ok {
				if msg, ok := m["message"].(string); ok {
					runErr = fmt.Errorf("%s", msg)
				}
			}
		}
	}
	if runErr != nil {
		return sid, finalText.String(), runErr
	}
	return sid, strings.TrimSpace(finalText.String()), nil
}

func nextRunAfter(t *ScheduledTask, now time.Time) int64 {
	switch t.Kind {
	case "once":
		return 0
	case "interval":
		if t.IntervalSec < 60 {
			return 0
		}
		next := t.NextRunAt
		if next == 0 {
			next = now.Add(time.Duration(t.IntervalSec) * time.Second).UnixMilli()
		} else {
			next += t.IntervalSec * 1000
		}
		if next <= now.UnixMilli() {
			next = now.Add(time.Duration(t.IntervalSec) * time.Second).UnixMilli()
		}
		return next
	case "daily", "weekdays", "custom":
		return nextCalendar(t, now)
	default:
		return 0
	}
}

func nextCalendar(t *ScheduledTask, now time.Time) int64 {
	loc := time.Local
	if t.Timezone != "" {
		if loaded, err := time.LoadLocation(t.Timezone); err == nil {
			loc = loaded
		}
	}
	if t.Days == nil && t.Kind == "weekdays" {
		t.Days = []int{1, 2, 3, 4, 5}
	}
	hour, minute := 0, 0
	if _, err := fmt.Sscanf(t.TimeOfDay, "%d:%d", &hour, &minute); err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0
	}
	allowed := func(d time.Weekday) bool {
		if t.Kind == "daily" {
			return true
		}
		for _, x := range t.Days {
			if x == int(d) {
				return true
			}
		}
		return false
	}
	for i := 0; i < 14; i++ {
		candidate := time.Date(now.Year(), now.Month(), now.Day()+i, hour, minute, 0, 0, loc)
		if candidate.After(now) && allowed(candidate.Weekday()) {
			return candidate.UnixMilli()
		}
	}
	return 0
}

func validateScheduled(t *ScheduledTask) error {
	if strings.TrimSpace(t.Label) == "" {
		return fmt.Errorf("label is required")
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	switch t.Kind {
	case "once":
		if t.At <= 0 {
			return fmt.Errorf("at is required for once")
		}
	case "interval":
		if t.IntervalSec < 60 {
			return fmt.Errorf("interval_sec must be at least 60")
		}
	case "daily", "weekdays", "custom":
		if t.TimeOfDay == "" {
			return fmt.Errorf("time is required")
		}
		if t.Kind == "custom" && len(t.Days) == 0 {
			return fmt.Errorf("days is required for custom")
		}
	default:
		return fmt.Errorf("kind must be once, interval, daily, weekdays or custom")
	}
	switch t.Target {
	case "", "new", "append":
	default:
		return fmt.Errorf("target must be new or append")
	}
	return nil
}

func (s *Scheduler) upsert(t *ScheduledTask) error {
	if err := validateScheduled(t); err != nil {
		return err
	}
	now := time.Now()
	if t.ID == "" {
		t.ID = newID("task")
	}
	if t.Target == "" {
		t.Target = "new"
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = now.UnixMilli()
	}
	if t.Model == "" {
		t.Model = s.cfg.DefaultModel
	}
	t.UpdatedAt = now.UnixMilli()
	if t.Enabled {
		switch t.Kind {
		case "once":
			t.NextRunAt = t.At
		case "interval":
			t.NextRunAt = now.Add(time.Duration(t.IntervalSec) * time.Second).UnixMilli()
		default:
			t.NextRunAt = nextCalendar(t, now)
		}
	}
	return s.store.Upsert(t)
}
