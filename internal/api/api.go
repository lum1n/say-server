// Package api serves the HTTP API under /api/v1, the widget loader at
// /widget.js and the embedded web app for every other path.
package api

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:static
var static embed.FS

// The SPA shell that TanStack Start prerenders in SPA mode. Every path that
// is not a file or an API route gets it, and the client router takes over.
const shell = "_shell.html"

type Server struct {
	version string
	assets  fs.FS
}

func New(version string) *Server {
	assets, _ := fs.Sub(static, "static")
	return &Server{version: version, assets: assets}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.Handle("/api/", http.NotFoundHandler())
	mux.HandleFunc("/", s.app)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) app(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" && name != shell {
		if f, err := s.assets.Open(name); err == nil {
			st, err := f.Stat()
			f.Close()
			if err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, s.assets, name)
				return
			}
		}
		if strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
	}
	body, err := fs.ReadFile(s.assets, shell)
	if err != nil {
		body = []byte(placeholder)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}

const placeholder = `<!doctype html><meta charset="utf-8"><title>say</title>
<p>The web app is not built into this binary. Run <code>make build</code>.</p>`

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
