package trace

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/proxy"
)

func mkEvent(model, prompt string) proxy.Event {
	req, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 64, "prompt": prompt})
	resp, _ := json.Marshal(map[string]any{"text": "answer to " + prompt})
	return proxy.Event{
		Time: time.Now(), Wallet: "w1", Model: model,
		Request: req, Response: resp,
		Usage: proxy.Usage{InputTokens: 10, OutputTokens: 5},
		Cost:  42, Balance: 958, Outcome: proxy.OutcomeOK,
	}
}

func TestWriteAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ep.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}

	w.Record(mkEvent("m", "one"))
	if err := w.Append(EventNote, map[string]string{"msg": "round 1 ends"}); err != nil {
		t.Fatal(err)
	}
	w.Record(mkEvent("m", "two"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	lines, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("read %d lines, want 3", len(lines))
	}
	for i, l := range lines {
		if l.Seq != int64(i+1) {
			t.Errorf("line %d has seq %d, want %d", i, l.Seq, i+1)
		}
	}

	calls, err := ModelCalls(lines)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("extracted %d model calls, want 2", len(calls))
	}
	if calls[0].Cost != 42 || calls[0].Wallet != "w1" {
		t.Errorf("call round-trip mangled: %+v", calls[0])
	}
}

func TestPlaybackReturnsRecordedResponsesExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ep.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ev1, ev2 := mkEvent("m", "one"), mkEvent("m", "two")
	w.Record(ev1)
	w.Record(ev2)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	pb, err := NewPlayback(path)
	if err != nil {
		t.Fatal(err)
	}
	if pb.Remaining() != 2 {
		t.Fatalf("remaining = %d, want 2", pb.Remaining())
	}

	resp, usage, err := pb.Invoke(context.Background(), "m", ev1.Request)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != string(ev1.Response) || usage != ev1.Usage {
		t.Error("playback returned different bytes than were recorded")
	}

	// A diverging request must fail loudly, not improvise.
	_, _, err = pb.Invoke(context.Background(), "m", []byte(`{"different":"request"}`))
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverging replay = %v, want divergence error", err)
	}
}

func TestPlaybackSkipsCallsThatNeverCompleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ep.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Record(mkEvent("m", "one"))
	refused := mkEvent("m", "never happened")
	refused.Outcome = proxy.OutcomeRefused
	refused.Response = nil
	w.Record(refused)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	pb, err := NewPlayback(path)
	if err != nil {
		t.Fatal(err)
	}
	if pb.Remaining() != 1 {
		t.Errorf("remaining = %d, want 1 (refusals have no response to replay)", pb.Remaining())
	}
}

func TestCompareAcceptsIdenticalContentAndRejectsDrift(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl"), filepath.Join(dir, "c.jsonl")

	ev := mkEvent("m", "one")
	for _, path := range []string{a, b} {
		w, err := NewWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		w.Record(ev) // same content, different wall-clock write times
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := Compare(a, b); err != nil {
		t.Errorf("identical content compared unequal: %v", err)
	}

	drift := ev
	drift.Response = []byte(`{"text":"a different answer"}`)
	w, err := NewWriter(c)
	if err != nil {
		t.Fatal(err)
	}
	w.Record(drift)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Compare(a, c); err == nil {
		t.Error("drifted response compared equal; the replay check would pass vacuously")
	}
}

// A service restarts over its own trace. OpenWriter carries the sequence on
// from the last line, so a reader sees one record and not two runs that both
// begin at one.
func TestOpenWriterAppendsAndContinuesTheSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-trace.jsonl")
	w, err := OpenWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(EventType("boot"), map[string]int{"run": 1})
	w.Append(EventType("boot"), map[string]int{"run": 1})
	w.Close()

	w, err = OpenWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(EventType("boot"), map[string]int{"run": 2})
	w.Close()

	lines, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[2].Seq != 3 {
		t.Errorf("after a restart: %d lines, last seq %d; want 3 and 3", len(lines), lines[len(lines)-1].Seq)
	}
}

// A resumed trace is the file cut back to the checkpoint's line and continued
// from there: what a killed process wrote past its last checkpoint is gone,
// and the next line takes the number the checkpoint's successor would have.
func TestResumeWriterCutsBackToTheSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := w.Append(EventNote, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if w.Seq() != 10 {
		t.Fatalf("seq = %d, want 10", w.Seq())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := ResumeWriter(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Append(EventNote, map[string]any{"n": "resumed"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	lines, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 8 || lines[7].Seq != 8 || string(lines[7].Payload) != `{"n":"resumed"}` {
		t.Fatalf("resumed file = %d lines, last %d %s; want 8 lines ending seq 8 resumed", len(lines), lines[len(lines)-1].Seq, lines[len(lines)-1].Payload)
	}
	if _, err := ResumeWriter(path, 20); err == nil {
		t.Fatal("a checkpoint ahead of its trace was accepted")
	}
}
