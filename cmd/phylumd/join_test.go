package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The door's contract: a knock waits for the town, the town's answer is the
// reply, and a knock after closing time is refused rather than left hanging.
func TestJoinHandler(t *testing.T) {
	joins := make(chan joinReq)
	done := make(chan struct{})
	h := joinHandler(joins, done)

	knock := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/guests", strings.NewReader(body)))
		return rec
	}

	t.Run("the town answers", func(t *testing.T) {
		go func() {
			req := <-joins
			if req.path != "/x/pilgrim.py" {
				t.Errorf("town was handed %q", req.path)
			}
			req.reply <- joinReply{ID: "pilgrim", Day: 2, Clock: "14:10"}
		}()
		rec := knock(`{"path":"/x/pilgrim.py"}`)
		var rep joinReply
		json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusOK || rep.ID != "pilgrim" || rep.Day != 2 || rep.Clock != "14:10" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the town refuses", func(t *testing.T) {
		go func() {
			req := <-joins
			req.reply <- joinReply{Err: "the name is taken"}
		}()
		rec := knock(`{"path":"/x/mira.py"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "taken") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("an empty knock", func(t *testing.T) {
		if rec := knock(`{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("after closing time", func(t *testing.T) {
		close(done)
		rec := knock(`{"path":"/x/late.py"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "closed") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})
}
