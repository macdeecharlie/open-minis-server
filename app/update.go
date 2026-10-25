package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "https"
	if r.TLS == nil && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "http") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (s *Server) githubRequest(r *http.Request, endpoint string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "open-minis-server/"+serverVersion)
	if s.cfg.Update.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Update.Token)
	}
	return s.http.client.Do(req)
}

func (s *Server) handleAppReleases(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Update.Repo == "" {
		writeError(w, http.StatusNotFound, "updates are not configured")
		return
	}
	perPage := 30
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			perPage = n
		}
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	key := fmt.Sprintf("%d/%d", perPage, page)
	cacheSeconds := s.cfg.Update.CacheSeconds
	if cacheSeconds <= 0 {
		cacheSeconds = 60
	}

	s.updateMu.Lock()
	entry, ok := s.updateCache[key]
	s.updateMu.Unlock()
	if ok && time.Now().Before(entry.expiresAt) {
		s.writeReleases(w, r, entry)
		return
	}

	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d&page=%d", s.cfg.Update.Repo, perPage, page)
	resp, err := s.githubRequest(r, endpoint, "application/vnd.github+json")
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	var releases []map[string]any
	if err := json.Unmarshal(body, &releases); err != nil {
		writeError(w, http.StatusBadGateway, "invalid GitHub response: "+err.Error())
		return
	}
	base := s.publicBase(r)
	for _, rel := range releases {
		tag, _ := rel["tag_name"].(string)
		if tag != "" {
			rel["html_url"] = base + "/v1/app/releases/" + url.PathEscape(tag)
		}
		assets, _ := rel["assets"].([]any)
		for _, a := range assets {
			asset, _ := a.(map[string]any)
			if asset == nil {
				continue
			}
			id := int64(0)
			switch v := asset["id"].(type) {
			case float64:
				id = int64(v)
			case json.Number:
				id, _ = v.Int64()
			}
			name, _ := asset["name"].(string)
			if id > 0 {
				asset["browser_download_url"] = fmt.Sprintf("%s/v1/app/assets/%d?name=%s", base, id, url.QueryEscape(name))
			}
		}
	}
	rewritten, err := json.Marshal(releases)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(rewritten)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	entry = updateCacheEntry{body: rewritten, etag: etag, expiresAt: time.Now().Add(time.Duration(cacheSeconds) * time.Second)}
	s.updateMu.Lock()
	s.updateCache[key] = entry
	s.updateMu.Unlock()
	s.writeReleases(w, r, entry)
}

func (s *Server) writeReleases(w http.ResponseWriter, r *http.Request, entry updateCacheEntry) {
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", entry.etag)
	w.Header().Set("Cache-Control", "private, max-age=30")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.body)
}

func (s *Server) handleAppAsset(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Update.Repo == "" {
		writeError(w, http.StatusNotFound, "updates are not configured")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid asset id")
		return
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", s.cfg.Update.Repo, id)
	resp, err := s.githubRequest(r, endpoint, "application/octet-stream")
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "minis-update.apk"
	}
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' || r == '/' {
			return '_'
		}
		return r
	}, name)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("asset stream: %v", err)
	}
}

func (s *Server) handleReleasePage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Update.Repo == "" {
		http.NotFound(w, r)
		return
	}
	tag := r.PathValue("tag")
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", s.cfg.Update.Repo, url.PathEscape(tag))
	resp, err := s.githubRequest(r, endpoint, "application/vnd.github+json")
	if err != nil {
		http.Error(w, "GitHub request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.NotFound(w, r)
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&rel); err != nil {
		http.Error(w, "invalid GitHub response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{font-family:system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 20px;line-height:1.5;color:#111}pre{white-space:pre-wrap;background:#f6f6f6;padding:16px;border-radius:10px}a{color:#06c}li{margin:6px 0}</style></head><body>`,
		html.EscapeString(rel.Name))
	fmt.Fprintf(w, `<h1>%s</h1><p><strong>%s</strong></p><pre>%s</pre><h2>Downloads</h2><ul>`, html.EscapeString(rel.Name), html.EscapeString(rel.TagName), html.EscapeString(rel.Body))
	for _, a := range rel.Assets {
		fmt.Fprintf(w, `<li>%s <span>(%d MB)</span></li>`, html.EscapeString(a.Name), a.Size/1024/1024)
	}
	fmt.Fprint(w, `</ul><p>Open the OpenMinis app and use Settings → Check for Updates to install this build. Asset downloads are authenticated.</p></body></html>`)
}
