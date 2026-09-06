package builder

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesThePageAndItsFiles(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for path, want := range map[string]string{
		"/":                   "<title>",
		"/?token=abc":         "<title>",
		"/static/builder.js":  "",
		"/static/builder.css": "",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body[:n]), want) {
			t.Errorf("%s: %d, %.80q", path, resp.StatusCode, body[:n])
		}
	}
	if resp, _ := http.Get(srv.URL + "/nothing-here"); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d, want 404", resp.StatusCode)
	}
}
