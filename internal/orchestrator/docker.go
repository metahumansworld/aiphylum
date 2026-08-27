// The Docker step runner: each step is one container behind the zero-egress
// network, with the proxy token in its environment and the observation on
// stdin. This is the only file in the package that knows containers exist;
// tests use in-process fakes against the same StepRunner interface.

package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/singhtushant3-hub/aiphylum/internal/runner"
)

// ContainerAgent describes how to run one registered agent's image.
type ContainerAgent struct {
	Image  string
	Cmd    []string
	Mounts []runner.Mount
	Memory string
	CPUs   string
}

// DockerSteps adapts runner.Runner to the StepRunner interface.
type DockerSteps struct {
	Runner *runner.Runner
	agents map[string]ContainerAgent
}

func NewDockerSteps(r *runner.Runner) *DockerSteps {
	return &DockerSteps{Runner: r, agents: map[string]ContainerAgent{}}
}

// Register binds an agent ID to its container image. Steps for unregistered
// agents fail as platform faults — the orchestrator should never have accepted
// the agent without an image, because such an agent livelocks any bounty it
// keeps winning (win → runner error → void → re-open, every round). The
// daemon upholds that: its intake binds the image, and checks it exists,
// before the agent gets a wallet.
func (d *DockerSteps) Register(agentID string, spec ContainerAgent) {
	d.agents[agentID] = spec
}

func (d *DockerSteps) RunStep(ctx context.Context, req StepRequest) (StepResult, error) {
	spec, ok := d.agents[req.AgentID]
	if !ok {
		return StepResult{}, fmt.Errorf("orchestrator: no container registered for agent %s", req.AgentID)
	}
	res, err := d.Runner.Run(ctx, runner.AgentSpec{
		Name:  "phylum-" + sanitize(req.Name),
		Image: spec.Image,
		Cmd:   spec.Cmd,
		Env: map[string]string{
			"PHYLUM_PROXY_URL": d.Runner.ProxyURL(),
			"PHYLUM_TOKEN":     req.Token,
		},
		Mounts:  spec.Mounts,
		Stdin:   req.Input,
		Timeout: req.Timeout,
		Memory:  spec.Memory,
		CPUs:    spec.CPUs,
	})
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		TimedOut: res.TimedOut,
	}, nil
}

// sanitize maps a step name onto Docker's container-name alphabet.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}
