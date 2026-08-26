// The subprocess step runner: each step is one host process with the proxy
// token in its environment and the observation on stdin. This is the demo's
// runner — no Docker, no isolation, so it is only for trusted reference
// agents (make demo). Untrusted code goes through DockerSteps.

package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// ProcessAgent describes how to run one registered agent as a subprocess.
type ProcessAgent struct {
	Cmd []string          // argv; e.g. {"python3", "generators/agents/frugal.py"}
	Env map[string]string // extra environment, e.g. PYTHONPATH
}

// ProcessSteps adapts host subprocesses to the StepRunner interface.
type ProcessSteps struct {
	// ProxyURL is the host-reachable base URL of the metering proxy — the
	// daemon's own HTTP server, not the Docker relay.
	ProxyURL string
	agents   map[string]ProcessAgent
}

func NewProcessSteps(proxyURL string) *ProcessSteps {
	return &ProcessSteps{ProxyURL: proxyURL, agents: map[string]ProcessAgent{}}
}

func (p *ProcessSteps) Register(agentID string, spec ProcessAgent) {
	p.agents[agentID] = spec
}

// RunStep runs one step to completion. Attribution is the whole game here:
// a timeout or a nonzero exit is the agent's problem and travels in the
// StepResult; an error return means the platform could not run the step at
// all (missing binary, cancelled episode) and voids the attempt.
func (p *ProcessSteps) RunStep(ctx context.Context, req StepRequest) (StepResult, error) {
	spec, ok := p.agents[req.AgentID]
	if !ok {
		return StepResult{}, fmt.Errorf("orchestrator: no process registered for agent %s", req.AgentID)
	}
	if len(spec.Cmd) == 0 {
		return StepResult{}, fmt.Errorf("orchestrator: empty command for agent %s", req.AgentID)
	}

	tctx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(tctx, spec.Cmd[0], spec.Cmd[1:]...)
	cmd.Stdin = bytes.NewReader(req.Input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env,
		"DUNGEON_PROXY_URL="+p.ProxyURL,
		"DUNGEON_TOKEN="+req.Token,
	)

	err := cmd.Run()
	res := StepResult{Stdout: stdout.String(), Stderr: stderr.String()}

	switch {
	case err == nil:
		return res, nil
	case tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		// The step's own wall clock expired: the agent's fault.
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	case ctx.Err() != nil:
		// The episode itself was cancelled: the platform's problem.
		return StepResult{}, ctx.Err()
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The process ran and failed: the agent's fault.
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		// Never started (missing interpreter, bad path): the platform's.
		return StepResult{}, fmt.Errorf("start %s step: %w", req.AgentID, err)
	}
}
