package town

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// A run picked up from a checkpoint writes exactly the lines the unbroken run
// wrote from that tick on — with the mind switched on, so the memories and
// the day each resident last turned in ride the state too. The state goes
// through JSON on the way, because that is how the daemon carries it.
func TestResumeFromAStateContinuesTheSameRun(t *testing.T) {
	m, people := AshmereFair()
	people = append(people, Guest("pilgrim", "Pilgrim", "a guest"))
	const at = 40

	var saved *State
	cfg := fast
	cfg.Days = 2
	cfg.Mind = &Minds{Provider: &proxy.StubProvider{}}
	cfg.Checkpoint = func(st State) error {
		if st.Tick != at {
			return nil
		}
		raw, err := json.Marshal(st)
		if err != nil {
			return err
		}
		saved = &State{}
		return json.Unmarshal(raw, saved)
	}
	whole := runInto(t, filepath.Join(t.TempDir(), "whole.jsonl"), m, people, cfg)
	if saved == nil {
		t.Fatalf("no checkpoint at tick %d", at)
	}
	if len(saved.Residents) != len(people) || saved.Mind == nil || len(saved.Mind.Streams) == 0 {
		t.Fatalf("state at tick %d: %d residents, mind %v", at, len(saved.Residents), saved.Mind)
	}

	// The tail of the unbroken run: everything after the at-th tick frame.
	ticks := 0
	var tail []string
	for _, l := range whole {
		if ticks >= at {
			tail = append(tail, string(l.Type)+" "+string(l.Payload))
		}
		var p struct{ Action string }
		json.Unmarshal(l.Payload, &p)
		if l.Type == trace.EventTown && p.Action == "tick" {
			ticks++
		}
	}

	again := cfg
	again.Checkpoint = nil
	again.From = saved
	tw, err := trace.NewWriter(filepath.Join(t.TempDir(), "again.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), tw, m, nil, again) // the roster is the state's
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	got, err := trace.Read(tw.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(tail) {
		t.Fatalf("resumed run wrote %d lines, the unbroken tail is %d", len(got), len(tail))
	}
	for i, l := range got {
		if have := string(l.Type) + " " + string(l.Payload); have != tail[i] {
			t.Fatalf("line %d differs\n  resumed:  %s\n  unbroken: %s", i+1, have, tail[i])
		}
	}
	if rep.Ticks != 2*DayMinutes/10 || rep.Reason != "day complete" {
		t.Fatalf("resumed report: %+v", rep)
	}
}
