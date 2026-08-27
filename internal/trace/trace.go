// Package trace persists what happened, verbatim.
//
// A trace is an append-only JSONL file of typed events. The proxy's records of
// model calls land here alongside the orchestrator's records of bids, awards,
// verifications and credit movements, in the order they happened. This file is
// the published artefact — traces are public while source stays private — and
// it is also the input to replay: playback re-reads the same bytes, so it is
// exact by construction rather than by re-execution.
package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/metahunmei/dungeon/internal/proxy"
)

// EventType tags each line so readers can decode the payload.
type EventType string

const (
	EventModelCall EventType = "model_call" // a proxy.Event: one metered call
	EventEpisode   EventType = "episode"    // episode lifecycle: start, end
	EventBounty    EventType = "bounty"     // posted, awarded, verified, failed
	EventBid       EventType = "bid"        // a sealed bid, revealed post-award
	EventCredit    EventType = "credit"     // mint, transfer, burn, payout
	EventAgent     EventType = "agent"      // spawned, retired, bankrupt
	EventSuite     EventType = "suite"      // an imported benchmark suite and its provenance
	EventNote      EventType = "note"       // free-form orchestrator annotation
	EventTown      EventType = "town"       // founded, tick, arrive, depart, met — the town track
)

// Line is one entry in a trace file.
type Line struct {
	Seq     int64           `json:"seq"`
	Time    time.Time       `json:"time"`
	Type    EventType       `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Writer appends events to a JSONL file. It is safe for concurrent use; the
// proxy and the orchestrator write to the same trace.
type Writer struct {
	mu   sync.Mutex
	f    *os.File
	bw   *bufio.Writer
	seq  int64
	path string
}

// NewWriter creates (or truncates) a trace file.
func NewWriter(path string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}
	return &Writer{f: f, bw: bufio.NewWriter(f), path: path}, nil
}

func (w *Writer) Path() string { return w.path }

// Append writes one event. The payload is marshalled once and stored verbatim.
func (w *Writer) Append(typ EventType, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", typ, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	line := Line{Seq: w.seq, Time: time.Now(), Type: typ, Payload: raw}
	enc, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("marshal trace line: %w", err)
	}
	if _, err := w.bw.Write(append(enc, '\n')); err != nil {
		return fmt.Errorf("write trace line: %w", err)
	}
	// Flush per line: a trace that lags the crash it should explain is not a
	// trace. The volume here is human-scale, not high-frequency.
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("flush trace: %w", err)
	}
	return nil
}

// Record implements proxy.Recorder, so a Writer can be handed straight to the
// proxy as its event sink.
func (w *Writer) Record(ev proxy.Event) {
	// A trace write failing must not fail the model call it describes; the
	// call has already been settled. Log-and-continue is the only sane choice,
	// and Append's error path is exercised directly in tests.
	if err := w.Append(EventModelCall, ev); err != nil {
		fmt.Fprintf(os.Stderr, "trace: dropped model_call event: %v\n", err)
	}
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// Read loads every line of a trace file, in order.
func Read(path string) ([]Line, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer f.Close()
	return ReadAll(f)
}

// ReadAll decodes a JSONL stream of trace lines.
func ReadAll(r io.Reader) ([]Line, error) {
	var out []Line
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20) // model calls carry full bodies
	for sc.Scan() {
		var line Line
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("trace line %d: %w", len(out)+1, err)
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan trace: %w", err)
	}
	return out, nil
}

// ModelCalls extracts the metered calls from a trace, decoded.
func ModelCalls(lines []Line) ([]proxy.Event, error) {
	var out []proxy.Event
	for _, l := range lines {
		if l.Type != EventModelCall {
			continue
		}
		var ev proxy.Event
		if err := json.Unmarshal(l.Payload, &ev); err != nil {
			return nil, fmt.Errorf("decode model_call seq %d: %w", l.Seq, err)
		}
		out = append(out, ev)
	}
	return out, nil
}
