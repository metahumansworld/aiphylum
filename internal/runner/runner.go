// Package runner owns agent containers and the zero-egress network they live on.
//
// Isolation is topology, not policy: agent containers sit on a Docker
// --internal network, which has no route to anywhere, and their DNS points at
// a dead resolver, so even name lookups go nowhere. The single reachable
// address is a relay container — dual-homed on the internal network and the
// default bridge — that forwards one port to the daemon's metering proxy on
// the host. An agent can therefore speak to exactly one thing in the world,
// and that thing meters it.
//
// Everything goes through the docker CLI. It is the stable interface, it needs
// no SDK dependency, and every command the runner issues can be replayed by a
// human debugging a container by hand.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Defaults for the demo world. All overridable on the Runner.
const (
	DefaultNetwork = "phylum-net"
	DefaultSubnet  = "172.28.0.0/16"
	DefaultRelayIP = "172.28.0.2"
	DefaultImage   = "python:3.12-slim"
	relayName      = "phylum-relay"
	relayPort      = 8080
	relayImage     = "alpine/socat"
)

// Runner manages the network, the relay, and agent containers.
type Runner struct {
	Network string
	Subnet  string
	RelayIP string
	Image   string
	Log     *slog.Logger
}

func New(log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		Network: DefaultNetwork,
		Subnet:  DefaultSubnet,
		RelayIP: DefaultRelayIP,
		Image:   DefaultImage,
		Log:     log,
	}
}

// ProxyURL is the one address an agent container can reach.
func (r *Runner) ProxyURL() string {
	return fmt.Sprintf("http://%s:%d", r.RelayIP, relayPort)
}

// docker runs one docker CLI command and returns its stdout.
func (r *Runner) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Available reports whether a usable docker daemon is on this machine.
func Available(ctx context.Context) bool {
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}

// ImageExists checks that an image is present locally. The daemon calls this
// at agent intake: an agent whose image cannot run would livelock every bounty
// it wins, so the time to find out is before it has a wallet, not mid-round.
// Deliberately no pull — what runs in the arena is what the operator loaded.
func (r *Runner) ImageExists(ctx context.Context, image string) error {
	if _, err := r.docker(ctx, "image", "inspect", image); err != nil {
		return fmt.Errorf("image %s not present locally: %w", image, err)
	}
	return nil
}

// EnsureNetwork creates the internal network if it does not exist.
func (r *Runner) EnsureNetwork(ctx context.Context) error {
	if _, err := r.docker(ctx, "network", "inspect", r.Network); err == nil {
		return nil
	}
	_, err := r.docker(ctx, "network", "create",
		"--internal", // the whole point: no uplink, no NAT, no way out
		"--subnet", r.Subnet,
		r.Network)
	if err != nil {
		return fmt.Errorf("create internal network: %w", err)
	}
	r.Log.Info("created zero-egress network", "network", r.Network, "subnet", r.Subnet)
	return nil
}

// StartRelay launches (or reuses) the socat relay that bridges the internal
// network to the host proxy. hostPort is where the daemon's proxy listens on
// the host. The relay is the custody boundary in physical form: it is the only
// container with a second leg on a routable network.
func (r *Runner) StartRelay(ctx context.Context, hostPort int) error {
	// A relay from a previous run may exist with a stale host port; replace it.
	if out, err := r.docker(ctx, "ps", "-aq", "--filter", "name=^"+relayName+"$"); err == nil && out != "" {
		if _, err := r.docker(ctx, "rm", "-f", relayName); err != nil {
			return fmt.Errorf("remove stale relay: %w", err)
		}
	}

	// Born on the default bridge so it can reach the host; host-gateway makes
	// host.docker.internal resolve on Linux as well as Docker Desktop.
	_, err := r.docker(ctx, "run", "-d",
		"--name", relayName,
		"--restart", "unless-stopped",
		"--add-host", "host.docker.internal:host-gateway",
		relayImage,
		fmt.Sprintf("tcp-listen:%d,fork,reuseaddr", relayPort),
		fmt.Sprintf("tcp:host.docker.internal:%d", hostPort),
	)
	if err != nil {
		return fmt.Errorf("start relay: %w", err)
	}

	// Second leg: a fixed address on the internal network, which is the one
	// URL agents are given.
	if _, err := r.docker(ctx, "network", "connect", "--ip", r.RelayIP, r.Network, relayName); err != nil {
		return fmt.Errorf("connect relay to internal network: %w", err)
	}
	r.Log.Info("relay up", "proxy_url", r.ProxyURL(), "host_port", hostPort)
	return nil
}

// StopRelay removes the relay container.
func (r *Runner) StopRelay(ctx context.Context) error {
	_, err := r.docker(ctx, "rm", "-f", relayName)
	return err
}

// AgentSpec describes one agent container run.
type AgentSpec struct {
	Name    string            // container name; also used to kill on timeout
	Image   string            // defaults to Runner.Image
	Cmd     []string          // command to run inside
	Env     map[string]string // PHYLUM_PROXY_URL and PHYLUM_TOKEN go here
	Mounts  []Mount           // agent source, SDK — read-only
	Stdin   []byte            // the step input, fed once at launch
	Timeout time.Duration     // wall-clock ceiling, fixed at bid time
	Memory  string            // e.g. "512m"
	CPUs    string            // e.g. "1"
}

// Mount is a read-only bind mount into the container.
type Mount struct {
	Host      string
	Container string
}

// Result is how one container run ended.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	Duration time.Duration
}

// Run executes an agent container to completion, enforcing the wall-clock
// ceiling. A timeout kills the container and reports TimedOut — the agent-side
// fault path; whatever the agent spent before the kill stays spent.
func (r *Runner) Run(ctx context.Context, spec AgentSpec) (Result, error) {
	image := spec.Image
	if image == "" {
		image = r.Image
	}

	args := []string{
		"run", "--rm",
		"--interactive", // the step input arrives on stdin
		"--name", spec.Name,
		"--network", r.Network,
		// A resolver that answers nothing: external names must not resolve,
		// and the proxy is addressed by IP so nothing needs to.
		"--dns", "127.0.0.1",
		// Least privilege; none of this is what an agent is here to use.
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--read-only",
		"--tmpfs", "/tmp:size=64m",
	}
	if spec.Memory != "" {
		args = append(args, "--memory", spec.Memory)
	}
	if spec.CPUs != "" {
		args = append(args, "--cpus", spec.CPUs)
	}
	for k, v := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	for _, m := range spec.Mounts {
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", m.Host, m.Container))
	}
	args = append(args, image)
	args = append(args, spec.Cmd...)

	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	start := time.Now()
	cmd := exec.CommandContext(runCtx, "docker", args...)
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if runCtx.Err() != nil && ctx.Err() == nil {
		// The wall-clock ceiling fired. CommandContext killed the docker
		// client; make sure the container itself is dead too.
		res.TimedOut = true
		killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer killCancel()
		if _, kerr := r.docker(killCtx, "kill", spec.Name); kerr != nil {
			r.Log.Warn("kill after timeout", "container", spec.Name, "err", kerr)
		}
		res.ExitCode = -1
		return res, nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The container ran and exited non-zero: that is the agent's
			// result, not a runner failure.
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("docker run %s: %w: %s", spec.Name, err, res.Stderr)
	}
	return res, nil
}
