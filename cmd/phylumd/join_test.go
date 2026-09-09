package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The door's contract, both ways: a knock waits for the town, the town's
// answer is the reply, and a knock after closing time is refused rather
// than left hanging.
func TestDoorHandler(t *testing.T) {
	joins := make(chan joinReq)
	leaves := make(chan leaveReq)
	done := make(chan struct{})
	h := doorHandler(joins, leaves, done)

	knock := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	join := func(body string) *httptest.ResponseRecorder { return knock("POST", "/v1/guests", body) }
	leave := func(id string) *httptest.ResponseRecorder { return knock("DELETE", "/v1/guests/"+id, "") }

	t.Run("the town seats", func(t *testing.T) {
		go func() {
			req := <-joins
			if req.path != "/x/pilgrim.py" {
				t.Errorf("town was handed %q", req.path)
			}
			req.reply <- doorReply{ID: "pilgrim", Day: 2, Clock: "14:10"}
		}()
		rec := join(`{"path":"/x/pilgrim.py"}`)
		var rep doorReply
		json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusOK || rep.ID != "pilgrim" || rep.Day != 2 || rep.Clock != "14:10" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("a ranked knock carries the enrolment both ways", func(t *testing.T) {
		go func() {
			req := <-joins
			if !req.ranked {
				t.Errorf("town was handed an unranked knock")
			}
			req.reply <- doorReply{ID: "pilgrim", Day: 1, Clock: "09:00", Ranked: true}
		}()
		rec := join(`{"path":"/x/pilgrim.py","ranked":true}`)
		var rep doorReply
		json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusOK || !rep.Ranked {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
		go func() {
			req := <-joins
			if req.ranked {
				t.Errorf("a plain knock arrived ranked")
			}
			req.reply <- doorReply{ID: "quiet", Day: 1, Clock: "09:10"}
		}()
		if rec := join(`{"path":"/x/quiet.py"}`); !strings.Contains(rec.Body.String(), `"id":"quiet"`) || strings.Contains(rec.Body.String(), "ranked") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the town refuses a seat", func(t *testing.T) {
		go func() {
			req := <-joins
			req.reply <- doorReply{Err: "the name is taken"}
		}()
		rec := join(`{"path":"/x/mira.py"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "taken") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("an empty knock", func(t *testing.T) {
		if rec := join(`{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the town lets one out", func(t *testing.T) {
		go func() {
			req := <-leaves
			if req.id != "pilgrim" {
				t.Errorf("town was handed %q", req.id)
			}
			req.reply <- doorReply{ID: "pilgrim", Day: 4, Clock: "09:40", Balance: 1730}
		}()
		rec := leave("pilgrim")
		var rep doorReply
		json.Unmarshal(rec.Body.Bytes(), &rep)
		if rec.Code != http.StatusOK || rep.ID != "pilgrim" || rep.Day != 4 || rep.Clock != "09:40" || rep.Balance != 1730 {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the town refuses to let one out", func(t *testing.T) {
		go func() {
			req := <-leaves
			req.reply <- doorReply{Err: "mira is not a guest"}
		}()
		rec := leave("mira")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a guest") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("after closing time", func(t *testing.T) {
		close(done)
		if rec := join(`{"path":"/x/late.py"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "closed") {
			t.Fatalf("join got %d %s", rec.Code, rec.Body)
		}
		if rec := leave("pilgrim"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "closed") {
			t.Fatalf("leave got %d %s", rec.Code, rec.Body)
		}
	})
}
