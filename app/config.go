package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	PublicURL    string        `json:"public_url"`
	AuthToken    string        `json:"auth_token"`
	DataDir      string        `json:"data_dir"`
	DefaultModel string        `json:"default_model"`
	Providers    []Provider    `json:"providers"`
	Update       UpdateConfig  `json:"update"`
	Sandbox      SandboxConfig `json:"sandbox"`
	SearXNGURL   string        `json:"searxng_url,omitempty"`
}

type Provider struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"` // openai | anthropic | mock
	BaseURL   string            `json:"base_url"`
	APIKey    string            `json:"api_key,omitempty"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Models    []Model           `json:"models"`
}

type Model struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	ContextWindow     int      `json:"context_window,omitempty"`
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	SupportsReasoning bool     `json:"supports_reasoning,omitempty"`
	InputModalities   []string `json:"input_modalities,omitempty"`
}

type UpdateConfig struct {
	Repo         string `json:"repo"`
	Token        string `json:"token,omitempty"`
	TokenEnv     string `json:"token_env,omitempty"`
	CacheSeconds int    `json:"cache_seconds,omitempty"`
}

type SandboxConfig struct {
	Mode          string `json:"mode,omitempty"` // "local" | "agent"
	AgentURL      string `json:"agent_url,omitempty"`
	AgentToken    string `json:"agent_token,omitempty"`
	AgentTokenEnv string `json:"agent_token_env,omitempty"`
	WorkRoot      string `json:"work_root,omitempty"`
	ShellUser     string `json:"shell_user,omitempty"`
	MaxOutput     int    `json:"max_output_bytes,omitempty"`
}

const (
	defaultDataDir     = "/data"
	defaultListen      = ":8080"
	defaultMaxOutput   = 200 * 1024
	defaultMaxTurns    = 500
	defaultUpdateCache = 60
)

// LoadConfig reads the JSON config and applies explicit environment
// overrides. It fails loudly on a missing token/provider rather than starting
// a half-configured server.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_AUTH_TOKEN")); v != "" {
		cfg.AuthToken = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_PUBLIC_URL")); v != "" {
		cfg.PublicURL = strings.TrimSuffix(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_DATA_DIR")); v != "" {
		cfg.DataDir = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_SEARXNG_URL")); v != "" {
		cfg.SearXNGURL = v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" && cfg.Update.Token == "" && cfg.Update.TokenEnv == "" {
		cfg.Update.Token = v
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	}
	if cfg.Update.CacheSeconds <= 0 {
		cfg.Update.CacheSeconds = defaultUpdateCache
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_MODE")); v != "" {
		cfg.Sandbox.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_AGENT_URL")); v != "" {
		cfg.Sandbox.AgentURL = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIS_SANDBOX_TOKEN")); v != "" && cfg.Sandbox.AgentToken == "" {
		cfg.Sandbox.AgentToken = v
	}
	if cfg.Sandbox.Mode == "" {
		cfg.Sandbox.Mode = "local"
	}
	if cfg.Sandbox.WorkRoot == "" {
		cfg.Sandbox.WorkRoot = filepath.Join(cfg.DataDir, "workspaces")
	}
	if cfg.Sandbox.ShellUser == "" {
		cfg.Sandbox.ShellUser = "minis"
	}
	if cfg.Sandbox.MaxOutput <= 0 {
		cfg.Sandbox.MaxOutput = defaultMaxOutput
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID == "" {
			p.ID = fmt.Sprintf("provider-%d", i+1)
		}
		if p.APIKey == "" && p.APIKeyEnv != "" {
			p.APIKey = strings.TrimSpace(os.Getenv(p.APIKeyEnv))
		}
	}
	if cfg.Update.Token == "" && cfg.Update.TokenEnv != "" {
		cfg.Update.Token = strings.TrimSpace(os.Getenv(cfg.Update.TokenEnv))
	}
	if cfg.Sandbox.AgentToken == "" && cfg.Sandbox.AgentTokenEnv != "" {
		cfg.Sandbox.AgentToken = strings.TrimSpace(os.Getenv(cfg.Sandbox.AgentTokenEnv))
	}
	return validateConfig(&cfg)
}

// validateConfig applies defaults and enforces the invariants shared by the
// startup loader and the authenticated admin config endpoint.
func validateConfig(cfg *Config) (*Config, error) {
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return nil, errors.New("auth_token is required (or set MINIS_AUTH_TOKEN)")
	}
	if cfg.PublicURL != "" && !strings.HasPrefix(cfg.PublicURL, "http://") && !strings.HasPrefix(cfg.PublicURL, "https://") {
		return nil, errors.New("public_url must start with http:// or https://")
	}
	seen := map[string]bool{}
	modelCount := 0
	for i, p := range cfg.Providers {
		if p.Kind != "openai" && p.Kind != "anthropic" && p.Kind != "gemini" && p.Kind != "mock" {
			return nil, fmt.Errorf("provider %q: kind must be openai, anthropic, gemini or mock", p.ID)
		}
		if p.Kind != "mock" && p.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required", p.ID)
		}
		if p.Kind == "openai" && p.APIKey == "" && len(p.Headers) == 0 {
			return nil, fmt.Errorf("provider %q: api_key is required", p.ID)
		}
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("provider %d (%s): at least one model is required", i+1, p.ID)
		}
		for _, m := range p.Models {
			if m.ID == "" {
				return nil, fmt.Errorf("provider %q: model id is required", p.ID)
			}
			if seen[m.ID] {
				return nil, fmt.Errorf("duplicate model id %q", m.ID)
			}
			seen[m.ID] = true
			modelCount++
		}
	}
	if cfg.DefaultModel == "" && modelCount > 0 {
		cfg.DefaultModel = cfg.Providers[0].Models[0].ID
	}
	if cfg.DefaultModel != "" && !seen[cfg.DefaultModel] {
		return nil, fmt.Errorf("default_model %q is not configured", cfg.DefaultModel)
	}
	if cfg.Sandbox.Mode != "local" && cfg.Sandbox.Mode != "agent" {
		return nil, errors.New("sandbox.mode must be local or agent")
	}
	if cfg.Sandbox.Mode == "agent" && cfg.Sandbox.AgentURL == "" {
		return nil, errors.New("sandbox.agent_url is required in agent mode")
	}
	return cfg, nil
}

// ResolveModel returns the provider and model definition for a requested id.
func (c *Config) ResolveModel(id string) (*Provider, *Model, error) {
	if id == "" {
		id = c.DefaultModel
	}
	if id == "" {
		return nil, nil, errors.New("no model configured")
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		for j := range p.Models {
			if p.Models[j].ID == id {
				return p, &p.Models[j], nil
			}
		}
	}
	return nil, nil, fmt.Errorf("unknown model %q", id)
}

func (c *Config) ModelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, p := range c.Providers {
		for _, m := range p.Models {
			item := map[string]any{
				"id":       m.ID,
				"object":   "model",
				"created":  1700000000,
				"owned_by": p.ID,
			}
			if m.Name != "" {
				item["name"] = m.Name
			}
			if len(m.InputModalities) > 0 {
				item["input_modalities"] = m.InputModalities
				item["architecture"] = map[string]any{
					"input_modalities":  m.InputModalities,
					"output_modalities": []string{"text"},
				}
			}
			if m.ContextWindow > 0 {
				item["context_window"] = m.ContextWindow
				item["context_length"] = m.ContextWindow
			}
			if m.MaxOutputTokens > 0 {
				item["max_output_tokens"] = m.MaxOutputTokens
			}
			if m.SupportsReasoning {
				item["supports_reasoning"] = true
			}
			out = append(out, item)
		}
	}
	// The Android bootstrap treats the first model as the new-chat default.
	// Keep the configured default first regardless of provider declaration
	// order, so adding an offline mock provider cannot silently become the
	// model every fresh install uses.
	if c.DefaultModel != "" {
		for i, item := range out {
			if item["id"] == c.DefaultModel && i > 0 {
				def := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = def
				break
			}
		}
	}
	return out
}

// ExampleConfig is written by the repo docs and is not used at runtime.
const ExampleConfig = `{
  "public_url": "https://minis.example.com",
  "auth_token": "replace-with-a-long-random-token",
  "default_model": "deepseek-v4-flash",
  "providers": [
    {
      "id": "deepseek",
      "kind": "openai",
      "base_url": "https://api.deepseek.com/v1",
      "api_key_env": "DEEPSEEK_API_KEY",
      "models": [
        {
          "id": "deepseek-v4-flash",
          "name": "DeepSeek V4 Flash",
          "context_window": 128000,
          "max_output_tokens": 8192,
          "supports_reasoning": true
        }
      ]
    }
  ],
  "update": {
    "repo": "macdeecharlie/open-minis-android",
    "token_env": "GITHUB_TOKEN"
  },
  "sandbox": {
    "work_root": "/data/workspaces",
    "shell_user": "minis",
    "max_output_bytes": 204800
  }
}`
