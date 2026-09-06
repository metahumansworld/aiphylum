package widget

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The widget's two routes sit on the same mux as the public surface and the
// builder, and must win against both without taking anything from them.
func TestWidgetRoutesWinOnTheSharedMux(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /widget.js", Handler())
	mux.Handle("GET /a/{id}/embed", Handler())
	mux.HandleFunc("/a/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("public")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("builder")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for path, want := range map[string]string{
		"/widget.js":         "data-agent",
		"/a/a_1/embed":       "<title>",
		"/a/a_1":             "public",
		"/a/a_1/messages":    "public",
		"/":                  "builder",
		"/static/builder.js": "builder",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body[:n]), want) {
			t.Errorf("%s: %d %.60q, want %q", path, resp.StatusCode, body[:n], want)
		}
	}
	if resp, _ := http.Get(srv.URL + "/a/a_1/embed/x"); resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		// Nothing under /embed/ is the widget's: it falls to the public
		// surface, whose fake here answers everything.
		t.Errorf("/a/a_1/embed/x: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
