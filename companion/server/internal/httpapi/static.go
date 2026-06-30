package httpapi

import (
	"io/fs"
	"net/http"
	"strings"

	"equip1/companion/server/web"
)

// distFS is the embedded SPA build rooted at the dist/ directory.
var distFS = mustSub(web.DistFS, "dist")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// handleStatic serves the embedded SPA, falling back to index.html for client
// routes (any non-/api, non-/health path that doesn't map to a real asset).
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// Never let the SPA shadow API/health routes.
	if strings.HasPrefix(path, "api/") || path == "health" {
		http.NotFound(w, r)
		return
	}

	if f, err := distFS.Open(path); err == nil {
		f.Close()
		http.FileServer(http.FS(distFS)).ServeHTTP(w, r)
		return
	}

	// SPA fallback: serve index.html.
	serveIndex(w, r)
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
