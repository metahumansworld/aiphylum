package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/spec"
)

type memRecorder struct {
	mu     sync.Mutex
	events []proxy.Event
}

func (m *memRecorder) Record(ev proxy.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *memRecorder) last(t *testing.T) proxy.Event {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 {
		t.Fatal("no events recorded")
	}
	return m.events[len(m.events)-1]
}

type faultProvider struct{}

func (faultProvider) Invoke(context.Context, string, []byte) ([]byte, proxy.Usage, error) {
	return nil, proxy.Usage{}, errors.New("provider is down")
}

// newService builds a service on the stub with the demo's price: one credit
// per token either way, so the numbers in the tests are readable.
func newService(t *testing.T, prov proxy.Provider, grant ledger.Credits) (*Service, *ledger.Ledger, *memRecorder) {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
	rec := &memRecorder{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := proxy.New(l, table, prov, rec, quiet)
	return New(Config{Proxy: p, Ledger: l, Grant: grant, Log: quiet}), l, rec
}

var steward = spec.Agent{
	Version:  1,
	Name:     "Tea Steward",
	Model:    "stub-1",
	Persona:  "The front-of-house voice of a small tea shop.",
	Greeting: "Welcome in. What can I pour you?",
	Rules:    []string{"Never quote a price."},
}

func TestAgentAnswersAsItselfAndPaysForIt(t *testing.T) {
	s, l, rec := newService(t, &proxy.StubProvider{}, 1_000_000)
	ctx := context.Background()
	ag, err := s.Create(ctx, steward)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	turn, err := s.Say(ctx, ag.ID, "", "Do you have any jasmine tea?")
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	for _, want := range []string{"Tea Steward", "jasmine", "Never quote a price"} {
		if !strings.Contains(turn.Reply, want) {
			t.Errorf("reply %q lacks %q", turn.Reply, want)
		}
	}
	if turn.Cost <= 0 {
		t.Errorf("a model call cost %d; the agent's wallet should have paid", turn.Cost)
	}
	bal, _ := l.Balance(ctx, ag.Wallet)
	if bal != 1_000_000-turn.Cost || turn.Balance != bal {
		t.Errorf("balance: wallet %d, turn says %d, grant was 1000000 and cost %d", bal, turn.Balance, turn.Cost)
	}

	// The proxy recorded the call as it records every call, and the request
	// it recorded is a service call: the spec's marker opens its system prompt.
	ev := rec.last(t)
	if ev.Outcome != proxy.OutcomeOK || ev.Wallet != ag.Wallet {
		t.Errorf("event = %+v, want ok on the agent's wallet", ev)
	}
	var req struct {
		System   string `json:"system"`
		Messages []message
	}
	if err := json.Unmarshal(ev.Request, &req); err != nil || !strings.HasPrefix(req.System, spec.Marker) {
		t.Errorf("recorded request is not a spec call: %s", ev.Request)
	}

	// The second message in the same conversation carries the first exchange.
	if _, err := s.Say(ctx, ag.ID, turn.Conversation, "And anything without caffeine?"); err != nil {
		t.Fatalf("second say: %v", err)
	}
	if err := json.Unmarshal(rec.last(t).Request, &req); err != nil || len(req.Messages) != 3 {
		t.Errorf("second call carried %d messages, want 3 (user, assistant, user)", len(req.Messages))
	}
}

func TestConversationsBelongToTheirAgent(t *testing.T) {
	s, _, _ := newService(t, &proxy.StubProvider{}, 1_000_000)
	ctx := context.Background()
	a, _ := s.Create(ctx, steward)
	porter := steward
	porter.Name = "Night Porter"
	b, _ := s.Create(ctx, porter)

	turn, err := s.Say(ctx, a.ID, "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Say(ctx, b.ID, turn.Conversation, "hello"); !errors.Is(err, ErrNoConversation) {
		t.Errorf("another agent continued a conversation it was not in: %v", err)
	}
	if _, err := s.Say(ctx, a.ID, "c_nope", "hello"); !errors.Is(err, ErrNoConversation) {
		t.Errorf("a made-up conversation id was accepted: %v", err)
	}
	if _, err := s.Say(ctx, "a_nope", "", "hello"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("a made-up agent id was accepted: %v", err)
	}
}

func TestCreateRefusesAModelNotOffered(t *testing.T) {
	s, _, _ := newService(t, &proxy.StubProvider{}, 1_000_000)
	a := steward
	a.Model = "anthropic/claude-opus-5"
	if _, err := s.Create(context.Background(), a); !errors.Is(err, ErrModelNotOffered) {
		t.Errorf("err = %v, want ErrModelNotOffered", err)
	}
	if got := len(s.Agents()); got != 0 {
		t.Errorf("%d agents exist after a refused create", got)
	}
}

// The grant is the cap. A wallet that cannot cover the worst case of the next
// call is refused by the proxy before any provider is touched, and the
// service says so in its own words, having charged nothing.
func TestOutOfCreditsIsTheProxysRefusalInTheServicesWords(t *testing.T) {
	// Worst case for one call here is roughly the body's bytes plus the reply
	// ceiling, about nine hundred credits; measured cost is a fraction of it.
	// A thousand credits buys exactly one message.
	s, l, rec := newService(t, &proxy.StubProvider{}, 1000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, steward)

	turn, err := s.Say(ctx, ag.ID, "", "hello")
	if err != nil {
		t.Fatalf("first say: %v", err)
	}
	before, _ := l.Balance(ctx, ag.Wallet)
	_, err = s.Say(ctx, ag.ID, turn.Conversation, "and again?")
	if !errors.Is(err, ErrOutOfCredits) {
		t.Fatalf("second say: err = %v, want ErrOutOfCredits", err)
	}
	after, _ := l.Balance(ctx, ag.Wallet)
	if after != before {
		t.Errorf("a refused call moved money: %d -> %d", before, after)
	}
	if ev := rec.last(t); ev.Outcome != proxy.OutcomeRefused {
		t.Errorf("proxy recorded %q, want refused", ev.Outcome)
	}
	// The unanswered message did not join the conversation.
	s.mu.Lock()
	n := len(s.convs[turn.Conversation].messages)
	s.mu.Unlock()
	if n != 2 {
		t.Errorf("conversation holds %d messages after a refusal, want 2", n)
	}
}

func TestProviderFaultChargesNothing(t *testing.T) {
	s, l, _ := newService(t, faultProvider{}, 1_000_000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, steward)
	if _, err := s.Say(ctx, ag.ID, "", "hello"); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("err = %v, want ErrProviderDown", err)
	}
	if bal, _ := l.Balance(ctx, ag.Wallet); bal != 1_000_000 {
		t.Errorf("balance %d after a provider fault, want the whole grant", bal)
	}
}

func TestHTTPSurfaces(t *testing.T) {
	s, _, _ := newService(t, &proxy.StubProvider{}, 1000)
	control := httptest.NewServer(s.Control())
	defer control.Close()
	public := httptest.NewServer(s.Public())
	defer public.Close()

	// Create over the control surface, from the JSON a builder would write.
	body := `{"version":1,"name":"Tea Steward","model":"stub-1","greeting":"Welcome in.","rules":["Never quote a price."]}`
	resp, err := http.Post(control.URL+"/v1/agents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created Agent
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.ID == "" {
		t.Fatalf("create: status %d, agent %+v", resp.StatusCode, created)
	}

	// A bad spec is a 400 with the reason, not a 500.
	resp, _ = http.Post(control.URL+"/v1/agents", "application/json", strings.NewReader(`{"version":1,"name":"x","model":"nope"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown model: status %d, want 400", resp.StatusCode)
	}

	// The public card is free and says only what a stranger should see.
	resp, _ = http.Get(public.URL + "/a/" + created.ID)
	var card map[string]any
	json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	if card["greeting"] != "Welcome in." || card["wallet"] != nil {
		t.Errorf("card = %v", card)
	}

	// One message is answered; the next meets the cap as a 402.
	post := func(text, conv string) (int, map[string]any) {
		raw, _ := json.Marshal(map[string]string{"text": text, "conversation": conv})
		resp, err := http.Post(public.URL+"/a/"+created.ID+"/messages", "application/json", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, turn := post("Do you have any jasmine tea?", "")
	reply, _ := turn["reply"].(string)
	conv, _ := turn["conversation"].(string)
	if status != http.StatusOK || !strings.Contains(reply, "jasmine") || conv == "" {
		t.Fatalf("message: status %d, turn %v", status, turn)
	}
	// Like the card, a reply says nothing about the agent's money.
	if _, leaked := turn["balance"]; leaked || turn["cost"] != nil {
		t.Errorf("public reply carries the agent's finances: %v", turn)
	}
	if status, _ := post("and again?", conv); status != http.StatusPaymentRequired {
		t.Errorf("out of credits: status %d, want 402", status)
	}
	// A fresh conversation is cheaper than a continued one, but a message near
	// the size limit is refused on what is left too — the cap is on the worst
	// case of the next call, not on a count of messages.
	if status, _ := post(strings.Repeat("tea ", 1000), ""); status != http.StatusPaymentRequired {
		t.Errorf("out of credits, new conversation: status %d, want 402", status)
	}
	if status, _ := post("", ""); status != http.StatusBadRequest {
		t.Errorf("empty message: status %d, want 400", status)
	}
}
