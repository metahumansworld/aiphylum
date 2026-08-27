package trace

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/singhtushant3-hub/aiphylum/internal/proxy"
)

// PlaybackProvider replays a recorded trace as if it were a live model.
//
// This is the "recorded playback — exact" mode: each request an agent makes is
// matched against the next recorded call, and the recorded response is
// returned byte for byte. If the agent is deterministic, re-running it against
// a PlaybackProvider reproduces the original episode exactly — and if its
// requests ever diverge from the record, playback says so instead of
// improvising, because a replay that silently invents responses is not a
// replay.
type PlaybackProvider struct {
	calls []proxy.Event
	next  int
}

// NewPlayback builds a playback provider from a recorded trace file.
func NewPlayback(path string) (*PlaybackProvider, error) {
	lines, err := Read(path)
	if err != nil {
		return nil, err
	}
	calls, err := ModelCalls(lines)
	if err != nil {
		return nil, err
	}
	// Only completed calls are replayable; refusals and faults produced no
	// response to play back.
	ok := calls[:0]
	for _, c := range calls {
		if c.Outcome == proxy.OutcomeOK {
			ok = append(ok, c)
		}
	}
	return &PlaybackProvider{calls: ok}, nil
}

// Remaining reports how many recorded calls have not yet been replayed. A
// finished exact replay ends at zero.
func (p *PlaybackProvider) Remaining() int { return len(p.calls) - p.next }

// Invoke implements proxy.Provider by returning the next recorded response.
func (p *PlaybackProvider) Invoke(_ context.Context, model string, body []byte) ([]byte, proxy.Usage, error) {
	if p.next >= len(p.calls) {
		return nil, proxy.Usage{}, fmt.Errorf("replay: agent made call %d but the record has only %d", p.next+1, len(p.calls))
	}
	rec := p.calls[p.next]
	if rec.Model != model || !bytes.Equal(rec.Request, body) {
		return nil, proxy.Usage{}, fmt.Errorf(
			"replay: call %d diverged from the record (recorded model %s, got %s); the agent is not deterministic or the trace is not its own",
			p.next+1, rec.Model, model)
	}
	p.next++
	return rec.Response, rec.Usage, nil
}

// Compare diffs two trace files call by call, for the byte-identical replay
// check. It compares the request and response bodies and usage of each metered
// call — the content of the episode — not timestamps, which legitimately
// differ between a run and its replay.
func Compare(originalPath, replayPath string) error {
	load := func(path string) ([]proxy.Event, error) {
		lines, err := Read(path)
		if err != nil {
			return nil, err
		}
		return ModelCalls(lines)
	}
	orig, err := load(originalPath)
	if err != nil {
		return fmt.Errorf("original: %w", err)
	}
	repl, err := load(replayPath)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	if len(orig) != len(repl) {
		return fmt.Errorf("replay has %d model calls, original has %d", len(repl), len(orig))
	}
	for i := range orig {
		o, r := orig[i], repl[i]
		if o.Model != r.Model {
			return fmt.Errorf("call %d: model %s vs %s", i+1, o.Model, r.Model)
		}
		if !bytes.Equal(o.Request, r.Request) {
			return fmt.Errorf("call %d: request bodies differ", i+1)
		}
		if !bytes.Equal(o.Response, r.Response) {
			return fmt.Errorf("call %d: response bodies differ", i+1)
		}
		if o.Usage != r.Usage {
			return fmt.Errorf("call %d: usage %+v vs %+v", i+1, o.Usage, r.Usage)
		}
		if o.Cost != r.Cost {
			return fmt.Errorf("call %d: cost %d vs %d", i+1, o.Cost, r.Cost)
		}
	}
	return nil
}

// MustExist is a small helper for CLI paths.
func MustExist(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("trace %s: %w", path, err)
	}
	return nil
}
