package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahunmei/dungeon/internal/trace"
)

type sseEvent struct {
	name string // "message" unless the frame names one
	id   string
	data string
}

// readSSE consumes one server-sent event from the stream.
func readSSE(t *testing.T, sc *bufio.Scanner) sseEvent {
	t.Helper()
	ev := sseEvent{name: "message"}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if ev.data != "" {
				return ev
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			ev.id = line[len("id: "):]
		case strings.HasPrefix(line, "data: "):
			ev.data = line[len("data: "):]
		case strings.HasPrefix(line, "event: "):
			ev.name = line[len("event: "):]
		}
	}
	t.Fatalf("event stream ended early: %v", sc.Err())
	return sseEvent{}
}

// TestLiveServerFollowsTheFile is milestone A's acceptance check: a live
// server pointed at a trace mid-write serves pages that update per request,
// streams new events over /events exactly as the static replay stream would
// clean them, and tells clients to start over when a new episode truncates
// the file under them.
func TestLiveServerFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ep.jsonl")

	// The server comes up before the episode does: empty world, no error.
	srv, err := NewLiveServer(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	get := func(p string) (int, string) {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
		return resp.StatusCode, sb.String()
	}

	// The badge's dot is drawn by the stylesheet rather than typed into the
	// markup, so the class is what the page actually asserts about itself.
	const liveBadge = `class="badge live"`
	if code, body := get("/"); code != 200 || !strings.Contains(body, liveBadge) {
		t.Fatalf("empty live overview: code=%d live badge present=%v", code, strings.Contains(body, liveBadge))
	}
	if _, body := get("/replay"); !strings.Contains(body, "window.LIVE = true") {
		t.Fatal("live replay page did not mark itself live")
	}

	// The episode starts: three events land before any spectator connects.
	w, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	emit := func(typ trace.EventType, payload map[string]any) {
		t.Helper()
		if err := w.Append(typ, payload); err != nil {
			t.Fatal(err)
		}
	}
	emit(trace.EventAgent, map[string]any{"action": "spawned", "agent": "a", "grant": 100})
	emit(trace.EventEpisode, map[string]any{"action": "start", "rounds": 1})
	emit(trace.EventBounty, map[string]any{"action": "posted", "id": "x", "generator": "arith", "seed": 7, "tier": 1, "max_payout": 30, "reserve": 3})

	// Pages rebuild per request — no restart, no cache to bust.
	if _, body := get("/"); !strings.Contains(body, `href="/agent/a"`) {
		t.Fatal("overview does not show the agent that just spawned")
	}

	// A spectator connects mid-episode and first receives the backlog.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)

	var streamed []sseEvent
	for range 3 {
		streamed = append(streamed, readSSE(t, sc))
	}

	// Two more events land while the stream is open — one of them a model
	// call whose body and clock must be stripped in flight.
	emit(trace.EventModelCall, map[string]any{"wallet": "att:x:r1", "model": "stub-1",
		"time":    "2026-08-27T12:00:09Z",
		"request": map[string]any{"model": "stub-1"}, "response": map[string]any{"text": "ok"},
		"usage": map[string]any{"input_tokens": 3, "output_tokens": 1}, "cost": 4, "balance": 96, "outcome": "ok"})
	emit(trace.EventBounty, map[string]any{"action": "solved", "id": "x", "agent": "a", "payout": 10, "burned": 4})
	for range 2 {
		streamed = append(streamed, readSSE(t, sc))
	}

	// The live feed and the static replay stream go through the same cleaning;
	// prove it by re-cleaning the finished file and comparing bytes.
	lines, err := trace.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != len(streamed) {
		t.Fatalf("streamed %d events, file has %d lines", len(streamed), len(lines))
	}
	for i, l := range lines {
		cleaned, err := cleanEvent(l)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(cleaned)
		if err != nil {
			t.Fatal(err)
		}
		if streamed[i].data != string(want) {
			t.Errorf("event %d: streamed %s, want %s", i, streamed[i].data, want)
		}
	}
	for _, k := range []string{`"request"`, `"response"`, `"time"`} {
		if strings.Contains(streamed[3].data, k) {
			t.Errorf("streamed model_call carries %s", k)
		}
	}

	// A new episode reuses the path: the file shrinks, the stream says reset.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if err := w2.Append(trace.EventAgent, map[string]any{"action": "spawned", "agent": "z", "grant": 5}); err != nil {
		t.Fatal(err)
	}
	if ev := readSSE(t, sc); ev.name != "reset" {
		t.Fatalf("after truncation got %q event, want reset", ev.name)
	}

	// Pages restart from the new file rather than mixing episodes.
	if _, body := get("/replay"); !strings.Contains(body, `"agent":"z"`) || strings.Contains(body, `"agent":"a"`) {
		t.Fatal("pages still show the old episode after truncation")
	}
}

// TestStaticServerHasNoEventFeed pins the split: a finished trace is
// immutable, so the static server neither polls the file nor offers a stream.
func TestStaticServerHasNoEventFeed(t *testing.T) {
	srv, err := NewServer("mini.jsonl", miniTrace(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/events", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("GET /events on a static server = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "text/event-stream") {
		t.Fatal("static server streamed events")
	}
}
