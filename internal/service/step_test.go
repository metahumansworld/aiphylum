package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/spec"
)

// bidder is a provider that answers every call with one actions line, the
// way a model that read the board protocol would: the test's stand-in for
// the model the stub is not.
type bidder struct{ line string }

func (b bidder) Invoke(_ context.Context, model string, _ []byte) ([]byte, proxy.Usage, error) {
	resp, _ := json.Marshal(map[string]any{
		"id": "msg_bidder", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": b.line}},
		"stop_reason": "end_turn",
		"usage":       proxy.Usage{InputTokens: 10, OutputTokens: 5},
	})
	return resp, proxy.Usage{InputTokens: 10, OutputTokens: 5}, nil
}

// TestStepIsPaidByTheTokenNotTheOwner pins the seam's one money rule: a
// step at the board is charged to whatever wallet the step's token names,
// and the owner's wallet — the one the agent's conversations spend — is
// untouched. The reply comes back whole, under the board prompt, with no
// history: two steps make two identical requests.
func TestStepIsPaidByTheTokenNotTheOwner(t *testing.T) {
	line := "PHYLUM_ACTIONS:{\"actions\":[{\"type\":\"bid\",\"bounty\":\"b1\",\"price\":30}]}"
	s, l, rec, owner := newService(t, bidder{line}, 1_000_000)
	ctx := context.Background()
	ag, err := s.Create(ctx, owner, steward)
	if err != nil {
		t.Fatal(err)
	}
	// The fair's wallet and the fair's token, minted the way the
	// orchestrator does it: the agent's id is its wallet.
	if err := l.CreateAccount(ctx, "fair-wallet", ledger.KindAgent, "fair-wallet"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "fair-wallet", 2000, "signing grant", "grant:fair-wallet"); err != nil {
		t.Fatal(err)
	}
	s.cfg.Proxy.Authorize("tok-step", "fair-wallet")

	input := []byte(`{"observation":{"phase":"bid","round":1},"wallet":{"id":"fair-wallet","balance":2000}}`)
	reply, err := s.Step(ctx, ag.ID, "tok-step", input)
	if err != nil {
		t.Fatal(err)
	}
	if reply != line {
		t.Fatalf("reply %q, want the provider's line back whole", reply)
	}
	ev := rec.last(t)
	if ev.Wallet != "fair-wallet" || ev.Cost != 15 {
		t.Errorf("charged %d to %q; want 15 to fair-wallet", ev.Cost, ev.Wallet)
	}
	if bal, _ := l.Balance(ctx, "fair-wallet"); bal != 2000-15 {
		t.Errorf("fair wallet %d, want %d", bal, 2000-15)
	}
	if bal, _ := l.Balance(ctx, owner.Wallet); bal != 1_000_000 {
		t.Errorf("owner's wallet %d moved; a step must not spend it", bal)
	}
	var req struct {
		System   string            `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(ev.Request, &req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.System, spec.Marker) || spec.Section(req.System, spec.SecEvents) == "" {
		t.Errorf("system prompt is not the board prompt:\n%s", req.System)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("%d messages in a step; want the one observation and no history", len(req.Messages))
	}
	first := ev.Request
	if _, err := s.Step(ctx, ag.ID, "tok-step", input); err != nil {
		t.Fatal(err)
	}
	if string(rec.last(t).Request) != string(first) {
		t.Errorf("second step's request differs from the first: a step carries no history")
	}

	// A token the proxy does not know is the step's fault, refused as a
	// person's message would be, and the owner still pays nothing.
	if _, err := s.Step(ctx, ag.ID, "tok-nobody", input); err == nil {
		t.Fatal("a step on an unknown token was answered")
	}
	if _, err := s.Step(ctx, "a_nobody", "tok-step", input); !errors.Is(err, ErrNoAgent) {
		t.Errorf("unknown agent: got %v, want ErrNoAgent", err)
	}
}
