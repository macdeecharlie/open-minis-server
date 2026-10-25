package main

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleScheduledList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.scheduledStore.List()})
}

func (s *Server) handleScheduledUpsert(w http.ResponseWriter, r *http.Request) {
	var task ScheduledTask
	if err := jsonDecodeLimited(w, r, &task, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if task.Target == "append" && !sessionIDPattern.MatchString(task.SessionID) {
		writeError(w, http.StatusBadRequest, "session_id is required and valid for target=append")
		return
	}
	if err := s.scheduler.upsert(&task); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.scheduler.Wake()
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleScheduledDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.scheduledStore.Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.scheduler.Wake()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScheduledRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.scheduledStore.Get(id) == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_ = s.scheduler.RunNow(ctx, id)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}
