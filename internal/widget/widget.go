// Package widget is the chat a stranger has with a built agent, on someone
// else's page. It is two files and nothing else: a loader a site owner
// pastes into their HTML, and the page the loader opens in a frame. The page
// is served from this platform, under the agent's own path, so its calls to
// the agent are same-origin and carry no session — it is the public surface
// with a face, and can do nothing the public surface cannot.
//
// The loader is deliberately dumb. It draws one button and one iframe and
// toggles the frame; everything that talks to the agent is inside the frame,
// on this origin, where the host page cannot read it and it cannot read the
// host page. A site that would rather have no frame calls the endpoint
// itself: the public surface answers cross-origin requests for that reason.
package widget

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

// Handler serves the loader at /widget.js and the chat page at
// /a/{id}/embed. Both patterns are more specific than the public surface's
// /a/ and the builder's /, so they win on a mux that carries all four.
func Handler() http.Handler {
	static, _ := fs.Sub(content, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widget.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "widget.js")
	})
	mux.HandleFunc("GET /a/{id}/embed", func(w http.ResponseWriter, r *http.Request) {
		// The page finds the agent in its own path; a missing agent is the
		// card's 404, shown by the page, so the frame never blanks.
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, static, "embed.html")
	})
	return mux
}
