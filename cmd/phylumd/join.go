// The fair's door. Boot seats whoever the flags named; this file lets a guest
// come later, while the week is running, and lets one go before it ends:
// phylumctl join hands a path to the listener on -listen, phylumctl leave
// hands a name, the listener hands either to the town, and the town acts on
// its next tick — seating the body through town.Config.Arrive, taking it off
// the roster through town.Config.Leave. Both are inward seams, money-free
// like Hold. Everything that makes a guest a guest (the filename rules, the
// wallet, the python3 process, the room at the tavern) is the same code boot
// uses, run on the town's goroutine at the tick the newcomer lands in, so a
// body joined before the first tick tells the same story as one seated at
// boot; and leaving is the fair's Leave, which freezes the wallet by never
// touching it again.
//
// Loopback and unauthenticated, like the live daemon's control plane: the
// wire carries a filesystem path phylumd will exec, and the checks on it run
// here, never in the client.
package main

import (
	"encoding/json"
	"net/http"

	"github.com/metahumansworld/soscitea/internal/ledger"
)

// joinReq is one guest at the door: the path phylumctl sent, and the channel
// the town answers on once it has seated or refused them.
type joinReq struct {
	path  string
	reply chan doorReply
}

// leaveReq is one guest going: the name, and the channel the town answers
// on once it has let them out or refused.
type leaveReq struct {
	id    string
	reply chan doorReply
}

// doorReply is the answer to either knock, and the wire shape of it: who
// was seated or let out and the minute it happened in, what they left
// with, or why not.
type doorReply struct {
	ID      string         `json:"id,omitempty"`
	Day     int            `json:"day,omitempty"`
	Clock   string         `json:"clock,omitempty"`
	Balance ledger.Credits `json:"balance,omitempty"`
	Err     string         `json:"error,omitempty"`
}

// doorHandler is the fair's whole control plane: POST /v1/guests {"path"}
// to come, DELETE /v1/guests/{id} to go. Each carries its request to the
// town goroutine and waits for the answer, which comes on the next tick,
// the only moment the roster changes. done closes when the week ends, so a
// knock after closing time is refused rather than left waiting on a tick
// that will never come.
func doorHandler(joins chan<- joinReq, leaves chan<- leaveReq, done <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/guests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			writeJSON(w, http.StatusBadRequest, doorReply{Err: `want {"path": "<guest.py>"}`})
			return
		}
		req := joinReq{path: body.Path, reply: make(chan doorReply, 1)}
		knock(w, r, joins, req, req.reply, done)
	})
	mux.HandleFunc("DELETE /v1/guests/{id}", func(w http.ResponseWriter, r *http.Request) {
		req := leaveReq{id: r.PathValue("id"), reply: make(chan doorReply, 1)}
		knock(w, r, leaves, req, req.reply, done)
	})
	return mux
}

// knock hands one request to the town and writes back what it says. The
// same wait for both verbs, closing-time race included.
func knock[T any](w http.ResponseWriter, r *http.Request, to chan<- T, req T, reply <-chan doorReply, done <-chan struct{}) {
	select {
	case to <- req:
	case <-done:
		writeJSON(w, http.StatusConflict, doorReply{Err: "the fair has closed"})
		return
	case <-r.Context().Done():
		return
	}
	select {
	case rep := <-reply:
		answer(w, rep)
	case <-done:
		// The town may have answered on its last tick and closed in the
		// same breath; an answer already written is still the answer.
		select {
		case rep := <-reply:
			answer(w, rep)
		default:
			writeJSON(w, http.StatusConflict, doorReply{Err: "the fair has closed"})
		}
	case <-r.Context().Done():
	}
}

func answer(w http.ResponseWriter, rep doorReply) {
	if rep.Err != "" {
		writeJSON(w, http.StatusBadRequest, rep)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
