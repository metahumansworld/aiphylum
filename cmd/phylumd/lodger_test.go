package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/orchestrator"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/service"
	"github.com/metahumansworld/soscitea/internal/spec"
)

// lineProvider answers every call with one fixed text: the model the stub
// is not, one that read the board protocol and bids.
type lineProvider struct{ line string }

func (p lineProvider) Invoke(_ context.Context, model string, _ []byte) ([]byte, proxy.Usage, error) {
	resp, _ := json.Marshal(map[string]any{
		"id": "msg_line", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": p.line}},
		"stop_reason": "end_turn",
		"usage":       proxy.Usage{InputTokens: 10, OutputTokens: 5},
	})
	return resp, proxy.Usage{InputTokens: 10, OutputTokens: 5}, nil
}

// TestLodgerRoster pins the intake: the same rules as a guest's, read off a
// spec file instead of a Python one, and the spec must be one the builder
// would keep.
func TestLodgerRoster(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `{"version":1,"name":"Tea Steward","model":"stub-1","rules":["Never quote a price."]}`

	ls, err := lodgerRoster([]string{mk("Tea_Steward.json", good)}, map[string]bool{"judge": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].id != "tea-steward" || ls[0].spec.Name != "Tea Steward" {
		t.Fatalf("got %+v", ls)
	}
	if ls[0].spec.MaxReplyTokens != spec.DefaultMaxReplyTokens {
		t.Errorf("a spec naming no ceiling got %d; want the default", ls[0].spec.MaxReplyTokens)
	}
	for _, bad := range []string{
		mk("badger.py", "# not a spec"),
		mk("nameless.json", `{"version":1,"model":"stub-1"}`),
		mk("judge.json", good),
	} {
		if _, err := lodgerRoster([]string{bad}, map[string]bool{"judge": true}); err == nil {
			t.Errorf("%s was seated", filepath.Base(bad))
		}
	}
}

// TestLodgerStepBecomesABid is the seam end to end: a spec the builder
// would keep, seated through the service, shown a bid step under the
// fair's token, and its reply read by the fair's own parser into a bid —
// charged to the fair wallet the token names. Everyone else's step still
// goes to the runner the fair had.
func TestLodgerStepBecomesABid(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	line := "Steward here.\nPHYLUM_ACTIONS:{\"actions\":[{\"type\":\"bid\",\"bounty\":\"b1\",\"price\":30}]}"
	p := proxy.New(l, table, lineProvider{line}, nil, quiet)
	svc, err := service.New(service.Config{Proxy: p, Ledger: l, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	// The fair's wallet for the lodger, the way AddAgent makes one.
	if err := l.CreateAccount(ctx, "steward", ledger.KindAgent, "steward"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "steward", 2000, "signing grant", "grant:steward"); err != nil {
		t.Fatal(err)
	}
	ag, err := svc.Create(ctx, service.Owner{ID: "steward", Wallet: "steward"},
		spec.Agent{Version: 1, Name: "Tea Steward", Model: "stub-1", MaxReplyTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	other := &recordingSteps{}
	steps := &lodgerSteps{svc: svc, ids: map[string]string{"steward": ag.ID}, other: other}
	p.Authorize("tok-steward-r1-bid", "steward")

	input, _ := json.Marshal(orchestrator.StepInput{Observation: orchestrator.Observation{Phase: "bid", Round: 1}})
	res, err := steps.RunStep(ctx, orchestrator.StepRequest{AgentID: "steward", Name: "steward-r1-bid",
		Token: "tok-steward-r1-bid", Input: input, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("step failed: %+v", res)
	}
	acts, err := orchestrator.ParseActions(res.Stdout)
	if err != nil {
		t.Fatalf("the fair could not read the lodger's reply %q: %v", res.Stdout, err)
	}
	if len(acts) != 1 || acts[0].Type != orchestrator.ActionBid || acts[0].Bounty != "b1" || acts[0].Price != 30 {
		t.Fatalf("actions %+v; want one bid of 30 on b1", acts)
	}
	if bal, _ := l.Balance(ctx, "steward"); bal != 2000-15 {
		t.Errorf("lodger's fair wallet %d; want 2000 less the 15 the call cost", bal)
	}

	// A token the fair never minted is the lodger's failure, not the runner's.
	res, err = steps.RunStep(ctx, orchestrator.StepRequest{AgentID: "steward", Name: "steward-r2-bid",
		Token: "tok-forged", Input: input, Timeout: time.Second})
	if err != nil || res.ExitCode == 0 {
		t.Errorf("forged token: err %v, result %+v; want a failed step and no runner error", err, res)
	}

	// The cast's steps go where they always went.
	if _, err := steps.RunStep(ctx, orchestrator.StepRequest{AgentID: "frugal", Name: "frugal-r1-bid"}); err != nil {
		t.Fatal(err)
	}
	if other.saw != "frugal-r1-bid" {
		t.Errorf("a cast step went to the service, not the process runner")
	}
	if strings.Contains(res.Stdout, spec.Marker) {
		t.Errorf("the system prompt leaked into stdout")
	}
}

type recordingSteps struct{ saw string }

func (r *recordingSteps) RunStep(_ context.Context, req orchestrator.StepRequest) (orchestrator.StepResult, error) {
	r.saw = req.Name
	return orchestrator.StepResult{}, nil
}
