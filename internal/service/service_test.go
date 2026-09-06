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
	"time"

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
// per token either way, so the numbers in the tests are readable. The
// returned Owner has a wallet funded with grant, the way a signed-in user's
// would be; the service itself mints nothing.
func newService(t *testing.T, prov proxy.Provider, grant ledger.Credits) (*Service, *ledger.Ledger, *memRecorder, Owner) {
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
	s := New(Config{Proxy: p, Ledger: l, Log: quiet, Locked: []string{"anthropic/claude-opus-5"}})
	return s, l, rec, fund(t, l, "ada", grant)
}

// fund makes an owner the way the account store does: a user wallet in the
// ledger, minted once.
func fund(t *testing.T, l *ledger.Ledger, id string, grant ledger.Credits) Owner {
	t.Helper()
	ctx := context.Background()
	o := Owner{ID: "u_" + id, Wallet: "usr:" + id}
	if err := l.CreateAccount(ctx, o.Wallet, ledger.KindUser, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, o.Wallet, grant, "grant", o.ID); err != nil {
		t.Fatal(err)
	}
	return o
}

// bearer is the test's authenticator: the Authorization header names the
// owner outright. The real one looks a session up; the surface cannot tell.
func bearer(owners ...Owner) Authenticator {
	return func(r *http.Request) (Owner, error) {
		h := r.Header.Get("Authorization")
		for _, o := range owners {
			if h == "Bearer "+o.ID {
				return o, nil
			}
		}
		return Owner{}, ErrUnauthenticated
	}
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
	s, l, rec, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	ctx := context.Background()
	ag, err := s.Create(ctx, owner, steward)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ag.Wallet != owner.Wallet || ag.Owner != owner.ID {
		t.Fatalf("agent = %+v, want it on its owner's wallet", ag)
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
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	ctx := context.Background()
	a, _ := s.Create(ctx, owner, steward)
	porter := steward
	porter.Name = "Night Porter"
	b, _ := s.Create(ctx, owner, porter)

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
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	a := steward
	a.Model = "anthropic/claude-opus-5" // shown in the catalogue, and locked
	if _, err := s.Create(context.Background(), owner, a); !errors.Is(err, ErrModelNotOffered) {
		t.Errorf("err = %v, want ErrModelNotOffered", err)
	}
	if got := len(s.Agents()); got != 0 {
		t.Errorf("%d agents exist after a refused create", got)
	}
	// And an owner whose wallet the ledger has never heard of has nowhere to
	// spend from, so there is nothing to run.
	if _, err := s.Create(context.Background(), Owner{ID: "u_ghost", Wallet: "usr:ghost"}, steward); err == nil {
		t.Error("an agent was created on a wallet that does not exist")
	}
}

// The grant is the cap. A wallet that cannot cover the worst case of the next
// call is refused by the proxy before any provider is touched, and the
// service says so in its own words, having charged nothing.
func TestOutOfCreditsIsTheProxysRefusalInTheServicesWords(t *testing.T) {
	// Worst case for one call here is roughly the body's bytes plus the reply
	// ceiling, about nine hundred credits; measured cost is a fraction of it.
	// A thousand credits buys exactly one message.
	s, l, rec, owner := newService(t, &proxy.StubProvider{}, 1000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)

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
	s, l, _, owner := newService(t, faultProvider{}, 1_000_000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)
	if _, err := s.Say(ctx, ag.ID, "", "hello"); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("err = %v, want ErrProviderDown", err)
	}
	if bal, _ := l.Balance(ctx, ag.Wallet); bal != 1_000_000 {
		t.Errorf("balance %d after a provider fault, want the whole grant", bal)
	}
}

// as sends a JSON request to a surface with an owner's bearer, or none.
func as(t *testing.T, owner Owner, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if owner.ID != "" {
		req.Header.Set("Authorization", "Bearer "+owner.ID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHTTPSurfaces(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 1000)
	control := httptest.NewServer(s.Control(bearer(owner)))
	defer control.Close()
	public := httptest.NewServer(s.Public())
	defer public.Close()

	// Create over the control surface, from the JSON a builder would write.
	body := `{"version":1,"name":"Tea Steward","model":"stub-1","greeting":"Welcome in.","rules":["Never quote a price."]}`
	if status, _ := as(t, Owner{}, "POST", control.URL+"/v1/agents", body); status != http.StatusUnauthorized {
		t.Fatalf("create without a session: status %d, want 401", status)
	}
	status, made := as(t, owner, "POST", control.URL+"/v1/agents", body)
	var created Agent
	created.ID, _ = made["id"].(string)
	if status != http.StatusCreated || created.ID == "" {
		t.Fatalf("create: status %d, agent %v", status, made)
	}

	// A bad spec is a 400 with the reason, not a 500.
	if status, _ := as(t, owner, "POST", control.URL+"/v1/agents", `{"version":1,"name":"x","model":"nope"}`); status != http.StatusBadRequest {
		t.Errorf("unknown model: status %d, want 400", status)
	}

	// The catalogue needs no session, and says what is locked.
	status, models := as(t, Owner{}, "GET", control.URL+"/v1/models", "")
	if status != http.StatusOK || len(models["offered"].([]any)) != 1 || models["locked"].([]any)[0] != "anthropic/claude-opus-5" {
		t.Errorf("models: status %d, %v", status, models)
	}

	// The public card is free and says only what a stranger should see.
	_, card := as(t, Owner{}, "GET", public.URL+"/a/"+created.ID, "")
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

func TestControlShowsAnOwnerOnlyTheirOwnAgents(t *testing.T) {
	s, l, _, ada := newService(t, &proxy.StubProvider{}, 1_000_000)
	bo := fund(t, l, "bo", 1_000_000)
	control := httptest.NewServer(s.Control(bearer(ada, bo)))
	defer control.Close()
	body := `{"version":1,"name":"Tea Steward","model":"stub-1"}`

	_, adas := as(t, ada, "POST", control.URL+"/v1/agents", body)
	_, bos := as(t, bo, "POST", control.URL+"/v1/agents", body)
	adaID, boID := adas["id"].(string), bos["id"].(string)

	_, listed := as(t, ada, "GET", control.URL+"/v1/agents", "")
	agents := listed["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["id"] != adaID {
		t.Errorf("ada's list = %v, want only her agent", agents)
	}
	if status, _ := as(t, ada, "GET", control.URL+"/v1/agents/"+boID, ""); status != http.StatusNotFound {
		t.Errorf("ada reading bo's agent: status %d, want 404", status)
	}
	if status, _ := as(t, bo, "GET", control.URL+"/v1/agents/"+boID, ""); status != http.StatusOK {
		t.Errorf("bo reading his own agent: status %d, want 200", status)
	}
	if status, _ := as(t, Owner{}, "GET", control.URL+"/v1/agents", ""); status != http.StatusUnauthorized {
		t.Errorf("listing without a session: status %d, want 401", status)
	}
}

// One person, one grant: two agents of one owner spend the same wallet and
// run dry together, whichever spent it.
func TestTwoAgentsOfOneOwnerShareOneGrant(t *testing.T) {
	s, l, _, owner := newService(t, &proxy.StubProvider{}, 1000)
	ctx := context.Background()
	a, _ := s.Create(ctx, owner, steward)
	porter := steward
	porter.Name = "Night Porter"
	b, _ := s.Create(ctx, owner, porter)

	// The first agent talks until the wallet refuses it. A message costs
	// about two hundred credits here, so that is a handful of turns.
	var spent int
	for ; spent < 20; spent++ {
		if _, err := s.Say(ctx, a.ID, "", "hello"); errors.Is(err, ErrOutOfCredits) {
			break
		} else if err != nil {
			t.Fatalf("first agent, message %d: %v", spent+1, err)
		}
	}
	if spent == 0 || spent == 20 {
		t.Fatalf("first agent answered %d messages on a thousand credits", spent)
	}
	// The second agent has answered nothing and is refused all the same.
	if _, err := s.Say(ctx, b.ID, "", "hello"); !errors.Is(err, ErrOutOfCredits) {
		t.Errorf("second agent after the first spent the grant: %v, want ErrOutOfCredits", err)
	}
	if bal, _ := l.Balance(ctx, owner.Wallet); bal > 1000-ledger.Credits(spent)*100 {
		t.Errorf("wallet holds %d after %d messages", bal, spent)
	}
}

func TestTheEleventhMessageInAMinuteIs429(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)
	public := httptest.NewServer(s.Public())
	defer public.Close()

	for i := 0; i < DefaultRate.Burst; i++ {
		if _, err := s.Say(ctx, ag.ID, "", "hello"); err != nil {
			t.Fatalf("message %d: %v", i+1, err)
		}
	}
	_, err := s.Say(ctx, ag.ID, "", "hello")
	var limited *RateLimitError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("eleventh message: %v, want a RateLimitError with a wait", err)
	}
	// Over HTTP the wait is a Retry-After header, in whole seconds.
	req, _ := http.NewRequest("POST", public.URL+"/a/"+ag.ID+"/messages", strings.NewReader(`{"text":"hello"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("status %d, Retry-After %q; want 429 with a wait", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// At twenty a minute, three seconds buys one more message, then no more.
	now = now.Add(3 * time.Second)
	if _, err := s.Say(ctx, ag.ID, "", "hello"); err != nil {
		t.Errorf("after the bucket refilled one: %v", err)
	}
	if _, err := s.Say(ctx, ag.ID, "", "hello"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("a second message on one refilled token: %v, want ErrRateLimited", err)
	}
	// Another agent has its own bucket: the limit is per endpoint, so a
	// flood at one agent does not silence the owner's others.
	other, _ := s.Create(ctx, owner, steward)
	if _, err := s.Say(ctx, other.ID, "", "hello"); err != nil {
		t.Errorf("the owner's other agent was limited too: %v", err)
	}
}

// Two messages on one conversation at once are answered one after the other,
// and the second call carries the first answer: the requests the proxy sees
// hold three messages and then five, never three and three.
func TestConcurrentMessagesOnOneConversationSeeEachOther(t *testing.T) {
	s, _, rec, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)
	turn, err := s.Say(ctx, ag.ID, "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Say(ctx, ag.ID, turn.Conversation, "and?"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var sizes []int
	rec.mu.Lock()
	for _, ev := range rec.events[1:] {
		var req struct{ Messages []message }
		json.Unmarshal(ev.Request, &req)
		sizes = append(sizes, len(req.Messages))
	}
	rec.mu.Unlock()
	if len(sizes) != 2 || sizes[0] != 3 || sizes[1] != 5 {
		t.Errorf("concurrent calls carried %v messages, want [3 5]", sizes)
	}
}

func TestAnAgentForgetsItsOldestConversationAtTheCap(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	s.cfg.Conversations = 2
	s.cfg.Rate = Rate{Burst: 100, PerMinute: 6000}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { now = now.Add(time.Second); return now }
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)

	first, _ := s.Say(ctx, ag.ID, "", "one")
	second, _ := s.Say(ctx, ag.ID, "", "two")
	// Touching the first makes the second the oldest.
	if _, err := s.Say(ctx, ag.ID, first.Conversation, "still here"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Say(ctx, ag.ID, "", "three"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Say(ctx, ag.ID, second.Conversation, "?"); !errors.Is(err, ErrNoConversation) {
		t.Errorf("the least recently used conversation survived the cap: %v", err)
	}
	if _, err := s.Say(ctx, ag.ID, first.Conversation, "?"); err != nil {
		t.Errorf("the recently used conversation was forgotten: %v", err)
	}
}
