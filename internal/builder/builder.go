// Package builder is the page a person builds an agent on: a node editor
// over the one spec document, a chat that turns plain language into edits
// of it, and a box to try the result through its public endpoint. It is
// static files and nothing else — the page talks to the service's control
// surface and the account store's routes, and the server that serves it
// knows nothing of what it does.
//
// The page keeps its session token in the browser's storage and sends it as
// a bearer, the way the SDK would, rather than in a cookie. That is the
// account package's choice carried through, and it settles a question the
// widget milestone might otherwise reopen: the widget attaches to the
// public surface, which has no session, so a platform cookie would buy
// nothing there, and without a cookie there is no cross-site request to
// forge. Everything the page renders from a spec goes in as text, never as
// markup, which is the other half of that.
//
// No build step. The page is one HTML file, one stylesheet and one script,
// served as they are from the binary, so the arena's rule holds here too:
// what runs is what is in the repository.
package builder

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

// Handler serves the page at / and its files under /static/. Anything else
// under / is the page too, so a link with a token in its query lands on it.
func Handler() http.Handler {
	static, _ := fs.Sub(content, "static")
	files := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, static, "index.html")
	})
	return mux
}
