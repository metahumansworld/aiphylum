package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The attribution mapping is the point of ProcessSteps: a crashing or
// hanging agent must come back as a StepResult (charged agent fault), and
// only a step the platform could not run at all may return an error (which
// voids the attempt). Getting this wrong silently inverts the demo's story.
func TestProcessStepsAttribution(t *testing.T) {
	ps := NewProcessSteps("http://127.0.0.1:0")
	ctx := context.Background()

	t.Run("nonzero exit is agent fault", func(t *testing.T) {
		ps.Register("crasher", ProcessAgent{Cmd: []string{"/bin/sh", "-c", "echo doomed; exit 3"}})
		res, err := ps.RunStep(ctx, StepRequest{AgentID: "crasher", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("exit 3 must not be a platform fault: %v", err)
		}
		if res.ExitCode != 3 || res.TimedOut {
			t.Fatalf("result = %+v, want ExitCode 3, not timed out", res)
		}
		if !strings.Contains(res.Stdout, "doomed") {
			t.Fatalf("stdout %q lost", res.Stdout)
		}
	})

	t.Run("timeout is agent fault", func(t *testing.T) {
		ps.Register("sleeper", ProcessAgent{Cmd: []string{"/bin/sh", "-c", "sleep 30"}})
		start := time.Now()
		res, err := ps.RunStep(ctx, StepRequest{AgentID: "sleeper", Timeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("timeout must not be a platform fault: %v", err)
		}
		if !res.TimedOut {
			t.Fatalf("result = %+v, want TimedOut", res)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("timeout did not kill the process (took %v)", elapsed)
		}
	})

	t.Run("cancelled episode is platform fault", func(t *testing.T) {
		ps.Register("victim", ProcessAgent{Cmd: []string{"/bin/sh", "-c", "sleep 30"}})
		cctx, cancel := context.WithCancel(ctx)
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		start := time.Now()
		_, err := ps.RunStep(cctx, StepRequest{AgentID: "victim", Timeout: time.Minute})
		if err == nil {
			t.Fatal("cancelled episode must surface as an error, not an agent fault")
		}
		// Asserting only the error let this case sit through the whole sleep
		// without complaining — it was the other half of a sixty-second
		// package. Cancelling has to actually stop the work.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("cancel did not stop the process (took %v)", elapsed)
		}
	})

	t.Run("missing binary is platform fault", func(t *testing.T) {
		ps.Register("ghost", ProcessAgent{Cmd: []string{"/no/such/interpreter"}})
		_, err := ps.RunStep(ctx, StepRequest{AgentID: "ghost", Timeout: time.Second})
		if err == nil {
			t.Fatal("unstartable step must be a platform fault")
		}
	})

	t.Run("unregistered agent is platform fault", func(t *testing.T) {
		_, err := ps.RunStep(ctx, StepRequest{AgentID: "stranger", Timeout: time.Second})
		if err == nil {
			t.Fatal("unregistered agent must be a platform fault")
		}
	})

	t.Run("env and stdin reach the process", func(t *testing.T) {
		ps.Register("echoer", ProcessAgent{
			Cmd: []string{"/bin/sh", "-c", `cat; echo "url=$PHYLUM_PROXY_URL tok=$PHYLUM_TOKEN x=$DEMO_EXTRA"`},
			Env: map[string]string{"DEMO_EXTRA": "42"},
		})
		res, err := ps.RunStep(ctx, StepRequest{
			AgentID: "echoer",
			Token:   "tok-abc",
			Input:   []byte("observation-bytes"),
			Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"observation-bytes", "url=http://127.0.0.1:0", "tok=tok-abc", "x=42"} {
			if !strings.Contains(res.Stdout, want) {
				t.Fatalf("stdout %q missing %q", res.Stdout, want)
			}
		}
	})
}
