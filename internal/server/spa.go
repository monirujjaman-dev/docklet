package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the built dashboard (web/ → internal/server/dist via `make web`).
// The directory always contains .gitkeep so the binary compiles without a web build.
//
//go:embed all:dist
var dist embed.FS

const notBuiltPage = `<!doctype html><html><head><meta charset="utf-8"><title>Docklet</title></head>
<body style="font-family:system-ui;padding:2rem"><h1>Docklet</h1>
<p>The dashboard has not been built into this binary. Run <code>make build</code>.</p></body></html>`

// spaHandler serves embedded static assets and falls back to index.html for
// client-side routes. Hashed assets get long-lived cache headers.
func spaHandler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	index, indexErr := fs.ReadFile(sub, "index.html")
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" && !strings.HasPrefix(name, ".") {
			if st, err := fs.Stat(sub, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if indexErr != nil {
			_, _ = w.Write([]byte(notBuiltPage))
			return
		}
		_, _ = w.Write(index)
	})
}
