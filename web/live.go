package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// The live server polls the trace file rather than watching it: the writer
// flushes per line, a poll is stdlib-only, and at spectator cadence 250ms is
// indistinguishable from instant.
const tailInterval = 250 * time.Millisecond

// NewLiveServer serves a trace that is still being written. Pages rebuild
// from the file on request, and /events streams new trace lines as they land,
// cleaned exactly like the embedded replay stream. The file not existing yet
// is fine — the empty world renders until the episode starts writing.
func NewLiveServer(path string) (*Server, error) {
	s, err := newServer(path, true)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// refreshLocked brings lines, view and replay up to date with the file.
// Only complete lines are consumed — the writer may be mid-append — and a
// shrunken file means a new episode reused the path, so the world restarts.
func (s *Server) refreshLocked() error {
	var size int64
	fi, err := os.Stat(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// not written yet; size stays 0
	case err != nil:
		return err
	default:
		size = fi.Size()
	}

	if size < s.offset {
		s.lines, s.offset, s.view = nil, 0, nil
	}
	if size == s.offset && s.view != nil {
		return nil
	}
	if size > s.offset {
		lines, off, err := trace.Tail(s.path, s.offset)
		if err != nil {
			return err
		}
		s.lines = append(s.lines, lines...)
		s.offset = off
	}
	return s.rebuildLocked()
}

// events is the live feed: server-sent events, one cleaned trace event per
// message, id'd by trace seq. ?after=N (or a reconnecting browser's
// Last-Event-ID) resumes past what the page already has. When the file is
// truncated under the stream — a new episode on the same path — the client
// gets a "reset" event and is expected to start over.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			after = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	reset := func() {
		fmt.Fprint(w, "event: reset\ndata: {}\n\n")
		fl.Flush()
	}

	var off, prevSeq int64
	for {
		fi, err := os.Stat(s.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// not written yet — keep waiting
		case err != nil:
			return
		case fi.Size() < off:
			reset()
			return
		case fi.Size() > off:
			lines, newOff, err := trace.Tail(s.path, off)
			if err != nil {
				return
			}
			off = newOff
			for _, l := range lines {
				// Seq strictly increases within one file; a step backwards
				// means truncation the size check couldn't see.
				if l.Seq <= prevSeq {
					reset()
					return
				}
				prevSeq = l.Seq
				if l.Seq <= after {
					continue
				}
				ev, err := cleanEvent(l)
				if err != nil {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					return
				}
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", l.Seq, data)
				after = l.Seq
			}
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(tailInterval):
		}
	}
}
