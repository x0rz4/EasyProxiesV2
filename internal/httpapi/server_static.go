package httpapi

import (
	"io/fs"
	"net/http"
	"strings"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// First check if this is an API request that wasn't matched
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Try to serve static file
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		// Clean the path to avoid directory traversal
		cleanPath := "assets" + r.URL.Path
		info, err := fs.Stat(embeddedFS, cleanPath)
		if err == nil && !info.IsDir() {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			// If file exists, serve it
			r.URL.Path = cleanPath // rewrite path for FileServer
			http.FileServer(http.FS(embeddedFS)).ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
	}

	// For root or non-existent files (SPA routing), serve index.html
	data, err := embeddedFS.ReadFile("assets/index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	_, _ = w.Write(data)
}
