// Lodgers: built agents taking lodgings at the fair. A guest is a Python
// file the fair runs as a process; a lodger is a spec file the fair runs
// through the service, the same service that answers for it on the builder
// page. The halves meet here, in one place: the fair still speaks the step
// protocol and nothing else, the service still makes one metered call and
// nothing else, and this file is the adapter between the two — a step
// runner that hands a lodger's steps to the service and everyone else's to
// the process runner it always had.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/metahumansworld/soscitea/internal/orchestrator"
	"github.com/metahumansworld/soscitea/internal/service"
	"github.com/metahumansworld/soscitea/internal/spec"
)

// lodger is one spec on its way to the fair: the id it will trade under —
// its filename, by the guests' rule — and the spec itself.
type lodger struct {
	id   string
	spec spec.Agent
}

// lodgerRoster reads the -lodger files. The filename becomes the name, as a
// guest's does, and the same alphabet and the same no-two-owners rule
// apply: the id is a wallet and a walker, and a lodger named for a guest
// would merge with it.
func lodgerRoster(paths []string, taken map[string]bool) ([]lodger, error) {
	var out []lodger
	for _, p := range paths {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, ".json") {
			return nil, fmt.Errorf("lodger %s: a lodger is an agent spec, a .json file the builder writes", p)
		}
		id := strings.ToLower(strings.TrimSuffix(base, ".json"))
		id = strings.ReplaceAll(id, "_", "-")
		if !guestID.MatchString(id) {
			return nil, fmt.Errorf("lodger %s: the filename becomes the agent's name and %q is not one — use 2-16 of a-z, 0-9, hyphen, starting with a letter", p, id)
		}
		if taken[id] {
			return nil, fmt.Errorf("lodger %s: the name %q is already taken in this world", p, id)
		}
		taken[id] = true
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("lodger %s: %w", p, err)
		}
		var a spec.Agent
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("lodger %s: %w", p, err)
		}
		if a.MaxReplyTokens == 0 {
			a.MaxReplyTokens = spec.DefaultMaxReplyTokens
		}
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("lodger %s: %w", p, err)
		}
		out = append(out, lodger{id: id, spec: a})
	}
	return out, nil
}

// lodgerSteps is the fair's step runner once a lodger is seated: a lodger's
// step goes to the service as one call under the step's own token, and any
// other agent's step goes where it always went. The reply is the step's
// stdout, so ParseActions reads a lodger exactly as it reads a guest — the
// sentinel line or nothing — and a model that answers in prose has stood at
// the board and said nothing, at its own cost, the guest's rule.
//
// A refusal from the proxy (no credit, no such model) is the lodger's fault,
// as an exit code is a guest's: it comes back as a failed step, not a
// runner error, so the fair traces it and moves on. Only the context's end
// is the platform's.
type lodgerSteps struct {
	svc   *service.Service
	ids   map[string]string // fair id → service agent id
	other orchestrator.StepRunner
}

func (l *lodgerSteps) RunStep(ctx context.Context, req orchestrator.StepRequest) (orchestrator.StepResult, error) {
	sid, ok := l.ids[req.AgentID]
	if !ok {
		return l.other.RunStep(ctx, req)
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	reply, err := l.svc.Step(ctx, sid, req.Token, req.Input)
	switch {
	case err == nil:
		return orchestrator.StepResult{Stdout: reply + "\n"}, nil
	case ctx.Err() != nil && ctx.Err() == context.DeadlineExceeded:
		return orchestrator.StepResult{Stderr: err.Error(), ExitCode: 1, TimedOut: true}, nil
	case ctx.Err() != nil:
		return orchestrator.StepResult{}, ctx.Err()
	default:
		return orchestrator.StepResult{Stderr: err.Error(), ExitCode: 1}, nil
	}
}
