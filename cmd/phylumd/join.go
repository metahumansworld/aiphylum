// The fair's door. Boot seats whoever the flags named; this file lets a guest
// come later, while the week is running: phylumctl join hands a path to the
// listener on -listen, the listener hands it to the town, and the town seats
// the body on its next tick through town.Config.Arrive — the second inward
// seam, money-free like Hold. Everything that makes a guest a guest (the
// filename rules, the wallet, the python3 process, the room at the tavern)
// is the same code boot uses, run on the town's goroutine at the tick the
// newcomer lands in, so a body joined before the first tick tells the same
// story as one seated at boot.
//
// Loopback and unauthenticated, like the live daemon's control plane: the
// wire carries a filesystem path phylumd will exec, and the checks on it run
// here, never in the client.
package main

import (
	"encoding/json"
	"net/http"
)

// joinReq is one guest at the door: the path phylumctl sent, and the channel
// the town answers on once it has seated or refused them.
type joinReq struct {
	path  string
	reply chan joinReply
}

// joinReply is the answer, and the wire shape of it: who was seated and the
// minute they were seated in, or why not.
type joinReply struct {
	ID    string `json:"id,omitempty"`
	Day   int    `json:"day,omitempty"`
	Clock string `json:"clock,omitempty"`
	Err   string `json:"error,omitempty"`
}

// joinHandler is the fair's whole control plane: POST /v1/guests {"path"}.
// It carries the request to the town goroutine and waits for the answer,
// which comes on the next tick, the only moment the roster changes. done
// closes when the week ends, so a knock after closing time is refused
// rather than left waiting on a tick that will never come.
func joinHandler(joins chan<- joinReq, done <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/guests", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			writeJSON(w, http.StatusBadRequest, joinReply{Err: `want {"path": "<guest.py>"}`})
			return
		}
		req := joinReq{path: body.Path, reply: make(chan joinReply, 1)}
		select {
		case joins <- req:
		case <-done:
			writeJSON(w, http.StatusConflict, joinReply{Err: "the fair has closed"})
			return
		case <-r.Context().Done():
			return
		}
		select {
		case rep := <-req.reply:
			answer(w, rep)
		case <-done:
			// The town may have seated them on its last tick and closed in
			// the same breath; an answer already written is still the answer.
			select {
			case rep := <-req.reply:
				answer(w, rep)
			default:
				writeJSON(w, http.StatusConflict, joinReply{Err: "the fair has closed"})
			}
		case <-r.Context().Done():
		}
	})
	return mux
}

func answer(w http.ResponseWriter, rep joinReply) {
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
