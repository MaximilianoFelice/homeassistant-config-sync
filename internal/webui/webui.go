// Package webui serves the web UI (built from ui/ with `mise run ui:build`)
// embedded in the binary. Without a build, it serves a short notice instead,
// so the Go code compiles and tests run without Node.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the UI files. Paths are relative, so it works under the
// ingress path prefix.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return notBuilt()
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return notBuilt()
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}

// Built reports whether the binary embeds a built UI.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

func notBuilt() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Git Config Sync: the web UI is not part of this build (run `mise run ui:build`). The API is under /api/v1/.\n"))
	})
}
