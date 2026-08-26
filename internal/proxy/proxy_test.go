package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/metahunmei/dungeon/internal/ledger"
)

// memRecorder collects events for assertion.
type memRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (m *memRecorder) Record(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *memRecorder) last(t *testing.T) Event {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 {
		t.Fatal("no events recorded")
	}
	return m.events[len(m.events)-1]
}

// faultProvider fails every call, standing in for a provider outage.
type faultProvider struct{}

func (faultProvider) Invoke(context.Context, string, []byte) ([]byte, Usage, error) {
	return nil, Usage{}, errors.New("provider is down")
}

func newTestProxy(t *testing.T, prov Provider) (*Proxy, *ledger.Ledger, *memRecorder) {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	table := NewPriceTable()
	// $3/M input, $15/M output in nano-USD per token.
	table.Set("test-model", Price{InputPerTok: 3000, OutputPerTok: 15000})

	rec := &memRecorder{}
	p := New(l, table, prov, rec, nil)
	return p, l, rec
}

func call(t *testing.T, p *Proxy, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w
}

func messagesBody(maxTokens int64) map[string]any {
	return map[string]any{
		"model":      "test-model",
		"max_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": "solve the bounty"},
		},
	}
}

func TestCallIsMeteredAndDebited(t *testing.T) {
	ctx := context.Background()
	p, l, rec := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 1_000_000, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")

	w := call(t, p, "tok1", messagesBody(256))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}

	ev := rec.last(t)
	if ev.Outcome != OutcomeOK {
		t.Fatalf("outcome = %s, want ok", ev.Outcome)
	}
	if ev.Cost <= 0 {
		t.Fatalf("cost = %d, want > 0", ev.Cost)
	}

	// The debit equals the recorded cost exactly, and it went to the provider
	// account — spent, not vanished.
	bal, err := l.Balance(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 1_000_000-ev.Cost {
		t.Errorf("balance = %d, want %d", bal, 1_000_000-ev.Cost)
	}
	c, err := l.Conservation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spent != ev.Cost {
		t.Errorf("spent = %d, want %d", c.Spent, ev.Cost)
	}
	if err := l.Verify(ctx); err != nil {
		t.Errorf("integrity after call: %v", err)
	}

	// The recorded cost matches the price table applied to reported usage —
	// metering accuracy, checked against the provider's own figures.
	price, _ := p.Table.Lookup("test-model")
	if want := price.Cost(ev.Usage.InputTokens, ev.Usage.OutputTokens); ev.Cost != want {
		t.Errorf("cost = %d, want %d from usage %+v", ev.Cost, want, ev.Usage)
	}
}

func TestEmptyWalletIsRefusedBeforeTheProviderIsCalled(t *testing.T) {
	ctx := context.Background()
	// A fault provider doubles as a tripwire: if the proxy contacts the
	// provider despite the empty wallet, the outcome would be platform_fault
	// rather than refused.
	p, l, rec := newTestProxy(t, faultProvider{})
	if err := l.CreateAccount(ctx, "poor", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "poor", 10, "tiny grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "poor")

	w := call(t, p, "tok1", messagesBody(100_000))
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body = %s", w.Code, w.Body)
	}
	if ev := rec.last(t); ev.Outcome != OutcomeRefused {
		t.Errorf("outcome = %s, want refused", ev.Outcome)
	}
	if bal, _ := l.Balance(ctx, "poor"); bal != 10 {
		t.Errorf("balance = %d, want 10 (a refusal moves nothing)", bal)
	}
	if err := l.Verify(ctx); err != nil {
		t.Errorf("integrity: %v", err)
	}
}

func TestProviderFaultChargesNothing(t *testing.T) {
	ctx := context.Background()
	p, l, rec := newTestProxy(t, faultProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 1_000_000, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")

	w := call(t, p, "tok1", messagesBody(256))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", w.Code, w.Body)
	}
	if ev := rec.last(t); ev.Outcome != OutcomePlatformFault {
		t.Errorf("outcome = %s, want platform_fault", ev.Outcome)
	}
	// The hold was released in full: platform faults are free to the agent.
	if bal, _ := l.Balance(ctx, "w1"); bal != 1_000_000 {
		t.Errorf("balance = %d, want 1000000", bal)
	}
	c, _ := l.Conservation(ctx)
	if c.Held != 0 || c.Spent != 0 {
		t.Errorf("held=%d spent=%d, want both 0 after a released hold", c.Held, c.Spent)
	}
	if err := l.Verify(ctx); err != nil {
		t.Errorf("integrity: %v", err)
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	ctx := context.Background()
	p, l, _ := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 1_000_000, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")

	body := messagesBody(256)
	body["model"] = "not-on-the-list"
	w := call(t, p, "tok1", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body)
	}
	if bal, _ := l.Balance(ctx, "w1"); bal != 1_000_000 {
		t.Errorf("balance = %d, want untouched", bal)
	}
}

func TestUnknownTokenIsRejected(t *testing.T) {
	p, _, _ := newTestProxy(t, &StubProvider{})
	w := call(t, p, "who-is-this", messagesBody(256))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	w = call(t, p, "", messagesBody(256))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", w.Code)
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	ctx := context.Background()
	p, l, _ := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 1_000_000, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")
	if w := call(t, p, "tok1", messagesBody(64)); w.Code != http.StatusOK {
		t.Fatalf("before revoke: status = %d", w.Code)
	}
	p.Revoke("tok1")
	if w := call(t, p, "tok1", messagesBody(64)); w.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: status = %d, want 401", w.Code)
	}
}

func TestPerCallCeilingRefusesOversizedCalls(t *testing.T) {
	ctx := context.Background()
	p, l, _ := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 100_000_000, "rich grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")
	p.PerCallCeiling = 1000

	// Rich wallet, but the single call's worst case is over the cap.
	w := call(t, p, "tok1", messagesBody(1_000_000))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body)
	}
	if bal, _ := l.Balance(ctx, "w1"); bal != 100_000_000 {
		t.Errorf("balance = %d, want untouched", bal)
	}
}

func TestStubProviderIsDeterministic(t *testing.T) {
	s := &StubProvider{}
	body, _ := json.Marshal(messagesBody(256))
	r1, u1, err := s.Invoke(context.Background(), "test-model", body)
	if err != nil {
		t.Fatal(err)
	}
	r2, u2, err := s.Invoke(context.Background(), "test-model", body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r1, r2) || u1 != u2 {
		t.Error("same request produced different responses; seeded replay depends on determinism")
	}
}

func TestCostRoundsUpNeverDown(t *testing.T) {
	p := Price{InputPerTok: 250, OutputPerTok: 1250} // $0.25/M in, $1.25/M out
	// 1 input token = 250 nano-USD = 0.25 micro → must round to 1 credit.
	if got := p.Cost(1, 0); got != 1 {
		t.Errorf("Cost(1,0) = %d, want 1 (round up)", got)
	}
	if got := p.Cost(4, 0); got != 1 {
		t.Errorf("Cost(4,0) = %d, want exactly 1", got)
	}
	if got := p.Cost(0, 0); got != 0 {
		t.Errorf("Cost(0,0) = %d, want 0", got)
	}
	if got := p.Cost(1_000_000, 1_000_000); got != ledger.Credits(250_000+1_250_000) {
		t.Errorf("Cost(1M,1M) = %d, want 1500000", got)
	}
}

func TestWalletEndpointReportsBalance(t *testing.T) {
	ctx := context.Background()
	p, l, _ := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 4242, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")

	req := httptest.NewRequest(http.MethodGet, "/v1/wallet", nil)
	req.Header.Set("Authorization", "Bearer tok1")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out struct {
		Wallet  string         `json:"wallet"`
		Balance ledger.Credits `json:"balance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Wallet != "w1" || out.Balance != 4242 {
		t.Errorf("wallet endpoint = %+v, want w1/4242", out)
	}
}

func TestManyConcurrentCallsConserveCredits(t *testing.T) {
	ctx := context.Background()
	p, l, rec := newTestProxy(t, &StubProvider{})
	if err := l.CreateAccount(ctx, "w1", ledger.KindAgent, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "w1", 10_000_000, "grant", ""); err != nil {
		t.Fatal(err)
	}
	p.Authorize("tok1", "w1")

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := messagesBody(64)
			body["messages"] = []map[string]any{{"role": "user", "content": fmt.Sprintf("call %d", i)}}
			call(t, p, "tok1", body)
		}(i)
	}
	wg.Wait()

	// Whatever raced, the book must balance and every event's cost must sum
	// to exactly what left the wallet.
	if err := l.Verify(ctx); err != nil {
		t.Fatalf("integrity after concurrent calls: %v", err)
	}
	var total ledger.Credits
	rec.mu.Lock()
	for _, ev := range rec.events {
		total += ev.Cost
	}
	rec.mu.Unlock()
	bal, _ := l.Balance(ctx, "w1")
	if bal != 10_000_000-total {
		t.Errorf("balance = %d, want %d (recorded costs must equal debits)", bal, 10_000_000-total)
	}
}
