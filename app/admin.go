package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// handleAdminConfig replaces the server configuration and exits so the
// container's restart policy starts the new configuration cleanly. It is
// authenticated with the same bearer token as the rest of the API. The
// endpoint exists so credentials can be provisioned without putting them on a
// command line or in a shell history.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		writeError(w, http.StatusNotImplemented, "configuration path is not available")
		return
	}
	var next Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024))
	if err := dec.Decode(&next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(next.AuthToken), []byte(s.cfg.AuthToken)) != 1 {
		writeError(w, http.StatusForbidden, "auth_token in the new config must match the current token")
		return
	}
	if next.DataDir == "" {
		next.DataDir = s.cfg.DataDir
	}
	if next.Sandbox.WorkRoot == "" {
		next.Sandbox.WorkRoot = filepath.Join(next.DataDir, "workspaces")
	}
	if next.Sandbox.GlobalRoot == "" {
		next.Sandbox.GlobalRoot = s.cfg.Sandbox.GlobalRoot
	}
	if next.Sandbox.ShellUser == "" {
		next.Sandbox.ShellUser = s.cfg.Sandbox.ShellUser
	}
	if next.Sandbox.MaxOutput <= 0 {
		next.Sandbox.MaxOutput = s.cfg.Sandbox.MaxOutput
	}
	if next.Sandbox.Mode == "" {
		next.Sandbox.Mode = s.cfg.Sandbox.Mode
	}
	if next.Sandbox.AgentURL == "" {
		next.Sandbox.AgentURL = s.cfg.Sandbox.AgentURL
	}
	if next.Sandbox.AgentToken == "" {
		next.Sandbox.AgentToken = s.cfg.Sandbox.AgentToken
	}
	if next.Sandbox.AgentTokenEnv == "" {
		next.Sandbox.AgentTokenEnv = s.cfg.Sandbox.AgentTokenEnv
	}
	if next.Update.CacheSeconds <= 0 {
		next.Update.CacheSeconds = s.cfg.Update.CacheSeconds
	}
	validated, err := validateConfig(&next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeConfigAndRestart(w, validated)
}

func (s *Server) writeConfigAndRestart(w http.ResponseWriter, validated *Config) {
	body, err := json.MarshalIndent(validated, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmpName, s.configPath); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "accepted",
		"restart": true,
		"message": "configuration written; process will exit so the container restarts",
	})
	s.cancelAllRuns()
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
}

// handleAdminProvider upserts one provider and optionally makes one of its
// models the default. Existing credentials for other providers are preserved.
func (s *Server) handleAdminProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider     Provider `json:"provider"`
		DefaultModel string   `json:"default_model"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	next := *s.cfg
	next.Providers = append([]Provider(nil), s.cfg.Providers...)
	replaced := false
	for i := range next.Providers {
		if next.Providers[i].ID == body.Provider.ID {
			next.Providers[i] = body.Provider
			replaced = true
			break
		}
	}
	if !replaced {
		next.Providers = append(next.Providers, body.Provider)
	}
	if body.DefaultModel != "" {
		next.DefaultModel = body.DefaultModel
	}
	validated, err := validateConfig(&next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeConfigAndRestart(w, validated)
}

// handleAdminUpdateToken stores the private-release GitHub token without ever
// putting it in a shell command or container environment.
func (s *Server) handleAdminUpdateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	next := *s.cfg
	next.Update.Token = body.Token
	next.Update.TokenEnv = ""
	validated, err := validateConfig(&next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeConfigAndRestart(w, validated)
}
