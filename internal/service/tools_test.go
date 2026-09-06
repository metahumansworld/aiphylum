package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/spec"
)

// insecure lets a test's tools reach the httptest server on this machine,
// which the policy would otherwise refuse at Save and at dial.
func insecure(s *Service) *Service {
	s.cfg.InsecureTools = true
	s.egress = egressClient(true)
	return s
}

func withTool(t spec.Tool) spec.Agent {
	a := steward
	a.Tools = []spec.Tool{t}
	return a
}

// The whole round trip on the stub: the model asks for the tool by name,
// the service calls the owner's endpoint with the declared parameter and
// nothing else, and the model's next reply relays what came back. Two
// metered calls, both on the owner's wallet, add up to the turn's cost.
func TestAnAgentCallsItsToolAndRelaysTheAnswer(t *testing.T) {
	var got *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tea":%q,"on_shelf":12}`, r.URL.Query().Get("tea"))
	}))
	defer api.Close()

	s, _, rec, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	insecure(s)
	ctx := context.Background()
	ag, err := s.Create(ctx, owner, withTool(spec.Tool{
		Name: "stock", Description: "How many of a tea are on the shelf.", URL: api.URL + "/stock?shop=corner",
		Params: []spec.Param{{Name: "tea", Description: "The tea, by name"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.Say(ctx, ag.ID, "", "how much stock is left?")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Method != "GET" || got.URL.Query().Get("shop") != "corner" || got.URL.Query().Get("tea") != "how much stock is left?" ||
		got.Header.Get("X-Phylum-Agent") != ag.ID {
		t.Errorf("the tool was called wrong: %v", got)
	}
	if !strings.Contains(turn.Reply, "I asked stock, and it said: HTTP 200") || !strings.Contains(turn.Reply, `"on_shelf":12`) {
		t.Errorf("reply: %q", turn.Reply)
	}
	rec.mu.Lock()
	n := len(rec.events)
	var sum int64
	for _, ev := range rec.events {
		sum += int64(ev.Cost)
	}
	rec.mu.Unlock()
	if n != 2 || int64(turn.Cost) != sum {
		t.Errorf("%d calls recorded costing %d; the turn says %d", n, sum, turn.Cost)
	}
	// The history keeps words, not blocks: the next message goes out on a
	// conversation the model can read from the top.
	if _, err := s.Say(ctx, ag.ID, turn.Conversation, "thanks"); err != nil {
		t.Errorf("the message after a tool call: %v", err)
	}
}

func TestAPostToolSendsDeclaredParamsAsJSONAndNothingElse(t *testing.T) {
	var body map[string]any
	var ctype string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte("noted"))
	}))
	defer api.Close()
	s, _, _, _ := newService(t, &proxy.StubProvider{}, 100_000_000)
	insecure(s)
	text, failed := s.callTool(context.Background(), "a_1", spec.Tool{
		Name: "note", Description: "d", URL: api.URL, Method: "POST",
		Params: []spec.Param{{Name: "text"}, {Name: "who"}},
	}, json.RawMessage(`{"text":"hello","who":7,"secret":"not declared"}`))
	if failed || text != "HTTP 200\nnoted" {
		t.Errorf("%q, failed=%v", text, failed)
	}
	if ctype != "application/json" || body["text"] != "hello" || body["who"] != "7" || body["secret"] != nil {
		t.Errorf("body %v (%s): want the two declared params, as strings", body, ctype)
	}
}

// What comes back is bounded, and a redirect is reported, not followed:
// the address the guard checked is the one that answered.
func TestAToolsAnswerIsClippedAndARedirectIsNotFollowed(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write([]byte(strings.Repeat("x", 3*MaxToolResultBytes)))
		case "/away":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer api.Close()
	s, _, _, _ := newService(t, &proxy.StubProvider{}, 100_000_000)
	insecure(s)
	call := func(path string) (string, bool) {
		return s.callTool(context.Background(), "a_1", spec.Tool{Name: "t", Description: "d", URL: api.URL + path}, nil)
	}
	if text, failed := call("/big"); failed || len(text) > MaxToolResultBytes+64 || !strings.HasSuffix(text, "(clipped)") {
		t.Errorf("/big: %d bytes, failed=%v, ends %q", len(text), failed, text[max(0, len(text)-20):])
	}
	if text, failed := call("/away"); !failed || !strings.HasPrefix(text, "HTTP 302") {
		t.Errorf("/away: %q, failed=%v", text, failed)
	}
	if text, failed := call("/x"); !failed || !strings.HasPrefix(text, "HTTP 418") {
		t.Errorf("/x: %q, failed=%v", text, failed)
	}
}

// The policy at Save: https to a public name, or not at all — unless the
// service was told it is on a developer's machine.
func TestAToolURLIsCheckedAtSave(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	ctx := context.Background()
	for _, u := range []string{
		"http://shop.example/stock",
		"https://localhost/stock",
		"https://stock.localhost/",
		"https://intranet/stock",
		"https://10.0.0.4/stock",
		"https://[::1]/stock",
		"https://169.254.169.254/latest/meta-data/",
	} {
		_, err := s.Create(ctx, owner, withTool(spec.Tool{Name: "t", Description: "d", URL: u}))
		if !errors.Is(err, ErrToolBlocked) {
			t.Errorf("%s: %v, want ErrToolBlocked", u, err)
		}
	}
	if _, err := s.Create(ctx, owner, withTool(spec.Tool{Name: "t", Description: "d", URL: "https://shop.example/stock"})); err != nil {
		t.Errorf("a public https URL: %v", err)
	}
	insecure(s)
	if _, err := s.Create(ctx, owner, withTool(spec.Tool{Name: "t", Description: "d", URL: "http://127.0.0.1:1/stock"})); err != nil {
		t.Errorf("insecure, a local http URL: %v", err)
	}
}

// The policy at dial: the address a name resolves to is checked, and the
// connection is made to that address. A name that passes at Save and
// resolves here is refused here.
func TestAToolIsRefusedAtDialWhenItsHostResolvesInside(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("inside")) }))
	defer api.Close()
	req, _ := http.NewRequest("GET", api.URL, nil)
	_, err := egressClient(false).Do(req)
	if !errors.Is(err, ErrToolBlocked) {
		t.Errorf("dialling loopback: %v, want ErrToolBlocked", err)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1":            true,
		"10.1.2.3":             true,
		"172.16.0.1":           true,
		"192.168.1.1":          true,
		"169.254.169.254":      true, // cloud metadata
		"100.64.0.1":           true, // carrier NAT
		"0.0.0.0":              true,
		"255.255.255.255":      true,
		"::1":                  true,
		"fd00::1":              true,
		"fe80::1":              true,
		"::ffff:10.0.0.1":      true, // IPv4 in IPv6 clothing
		"::ffff:169.254.1.1":   true,
		"64:ff9b::a00:1":       true, // NAT64 of 10.0.0.1
		"8.8.8.8":              false,
		"2606:4700:4700::1111": false,
	} {
		if got := blocked(netip.MustParseAddr(addr)); got != want {
			t.Errorf("blocked(%s) = %v, want %v", addr, got, want)
		}
	}
}

// askingProvider always asks for a tool unless told it may not: the rounds
// a message may go must end, and end in words.
type askingProvider struct{ calls int }

func (p *askingProvider) Invoke(_ context.Context, model string, body []byte) ([]byte, proxy.Usage, error) {
	p.calls++
	var req struct {
		ToolChoice struct{ Type string } `json:"tool_choice"`
	}
	json.Unmarshal(body, &req)
	content := `[{"type":"text","text":"Once more."},{"type":"tool_use","id":"toolu_1","name":"t","input":{}}]`
	if req.ToolChoice.Type == "none" {
		content = `[{"type":"text","text":"Fine, here is my answer."}]`
	}
	out := fmt.Sprintf(`{"id":"m","type":"message","role":"assistant","model":%q,"content":%s,"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, model, content)
	return []byte(out), proxy.Usage{InputTokens: 10, OutputTokens: 5}, nil
}

func TestAMessageMayOnlyGoRoundItsToolsSoManyTimes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer api.Close()
	prov := &askingProvider{}
	s, _, _, owner := newService(t, prov, 100_000_000)
	insecure(s)
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, withTool(spec.Tool{Name: "t", Description: "d", URL: api.URL}))
	turn, err := s.Say(ctx, ag.ID, "", "go on then")
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != MaxToolRounds+1 || turn.Reply != "Fine, here is my answer." {
		t.Errorf("%d calls, reply %q; want %d calls ending in words", prov.calls, turn.Reply, MaxToolRounds+1)
	}
	// A tool the model made up is answered as an error, not called: the
	// endpoint of an agent whose only tool has another name is never hit.
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	ag2, _ := s.Create(ctx, owner, withTool(spec.Tool{Name: "u", Description: "d", URL: other.URL}))
	if turn, err := s.Say(ctx, ag2.ID, "", "again"); err != nil || hits != 0 || turn.Reply == "" {
		t.Errorf("made-up tool: %v, %d hits, reply %q", err, hits, turn.Reply)
	}
}
