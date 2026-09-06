package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/spec"
)

func TestAgentsOutliveTheProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.db")

	// Both services share one ledger only by accident of newService; what
	// matters is the store path. Build on the first, restart on the second.
	s1, l, _, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s1.cfg
	cfg.Store = st
	s1, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := s1.Create(ctx, owner, steward)
	if err != nil {
		t.Fatal(err)
	}
	revised := steward
	revised.Rules = append(revised.Rules, "Mention the weather.")
	if _, err := s1.Update(ctx, ag.ID, revised); err != nil {
		t.Fatal(err)
	}
	gone, _ := s1.Create(ctx, owner, steward)
	if err := s1.Delete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	cfg.Store = st2
	cfg.Ledger = l
	s2, err := New(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	back := s2.AgentsOf(owner.ID)
	if len(back) != 1 || back[0].ID != ag.ID {
		t.Fatalf("after restart: %d agents, want the one that was not deleted", len(back))
	}
	if got := back[0].Spec.Rules; len(got) != 2 || got[1] != "Mention the weather." {
		t.Errorf("restart lost the update: rules = %q", got)
	}
	if !back[0].Created.Equal(ag.Created) {
		t.Errorf("created moved across the restart: %v -> %v", ag.Created, back[0].Created)
	}
	// It answers, on a token this process made.
	if _, err := s2.Say(ctx, ag.ID, "", "any oolong?"); err != nil {
		t.Errorf("restarted agent does not answer: %v", err)
	}
	// And the deleted one's token is gone from the proxy: its old endpoint
	// on the old process would have been refused too, but that process is
	// gone, so ask the new one.
	if _, err := s2.Say(ctx, gone.ID, "", "hello?"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("deleted agent answers after restart: %v", err)
	}
}

func TestUpdateAndDeleteAreTheOwnersAlone(t *testing.T) {
	s, l, _, ada := newService(t, &proxy.StubProvider{}, 1_000_000)
	bea := fund(t, l, "bea", 1_000_000)
	control := httptest.NewServer(s.Control(bearer(ada, bea)))
	defer control.Close()
	ctx := context.Background()
	ag, _ := s.Create(ctx, ada, steward)
	url := control.URL + "/v1/agents/" + ag.ID
	body := `{"version":1,"name":"Tea Steward","model":"stub-1","rules":["Never quote a price.","Close at six."]}`

	if code, _ := as(t, bea, "PUT", url, body); code != 404 {
		t.Errorf("another owner's PUT: %d, want 404", code)
	}
	if code, _ := as(t, bea, "DELETE", url, ""); code != 404 {
		t.Errorf("another owner's DELETE: %d, want 404", code)
	}
	if code, _ := as(t, Owner{}, "PUT", url, body); code != 401 {
		t.Errorf("signed-out PUT: %d, want 401", code)
	}
	locked := strings.Replace(body, "stub-1", "anthropic/claude-opus-5", 1)
	if code, out := as(t, ada, "PUT", url, locked); code != 400 {
		t.Errorf("PUT to a locked model: %d %v, want 400", code, out)
	}
	code, out := as(t, ada, "PUT", url, body)
	if code != 200 {
		t.Fatalf("owner's PUT: %d %v", code, out)
	}
	if got, _ := s.Get(ag.ID); len(got.Spec.Rules) != 2 {
		t.Errorf("update did not land: rules = %q", got.Spec.Rules)
	}
	if code, _ := as(t, ada, "DELETE", url, ""); code != 204 {
		t.Errorf("owner's DELETE: %d, want 204", code)
	}
	if code, _ := as(t, ada, "GET", url, ""); code != 404 {
		t.Errorf("deleted agent is still there: %d", code)
	}
}

func TestUpdateEndsConversationsAndNotTheMessageInFlight(t *testing.T) {
	s, _, rec, owner := newService(t, &proxy.StubProvider{Latency: 0}, 1_000_000)
	ctx := context.Background()
	ag, _ := s.Create(ctx, owner, steward)
	turn, err := s.Say(ctx, ag.ID, "", "hello")
	if err != nil {
		t.Fatal(err)
	}

	// Messages and updates land together. Under -race this is the test
	// that a call reads the spec it was sent to and not one being swapped.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			revised := steward
			revised.Name = fmt.Sprintf("Steward %d", i)
			if _, err := s.Update(ctx, ag.ID, revised); err != nil {
				t.Error(err)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, err := s.Say(ctx, ag.ID, "", "still there?"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := s.Say(ctx, ag.ID, turn.Conversation, "and again?"); !errors.Is(err, ErrNoConversation) {
		t.Errorf("the old conversation survived an update: %v", err)
	}
	if ev := rec.last(t); ev.Outcome != proxy.OutcomeOK {
		t.Errorf("last call: %q", ev.Outcome)
	}
}

func TestDraftRevisesOnTheOwnersGrant(t *testing.T) {
	s, l, rec, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	ctx := context.Background()
	before, _ := l.Balance(ctx, owner.Wallet)

	// From nothing: no name, no model. The draft names it and the service
	// pins the model to the builder's.
	d, err := s.Draft(ctx, owner, spec.Agent{}, "a steward for a small tea shop, who never quotes a price")
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if d.Spec.Name == "" || d.Spec.Model != "stub-1" || d.Spec.Version != spec.Version {
		t.Errorf("draft = %+v, want a named agent on the builder's model", d.Spec)
	}
	if !strings.Contains(strings.Join(d.Spec.Rules, "\n"), "never quotes a price") {
		t.Errorf("the request did not reach the rules: %q", d.Spec.Rules)
	}
	if d.Note == "" {
		t.Error("no note")
	}
	after, _ := l.Balance(ctx, owner.Wallet)
	if d.Cost <= 0 || after != before-d.Cost || d.Balance != after {
		t.Errorf("cost %d, balance %d -> %d, reported %d", d.Cost, before, after, d.Balance)
	}
	if ev := rec.last(t); ev.Model != "stub-1" || ev.Outcome != proxy.OutcomeOK {
		t.Errorf("proxy recorded %+v", ev)
	}

	// From an agent on a model: the model is pinned even if the draft would
	// move it. Ask the stub for it by hand, since the stub never moves it.
	cur := steward
	cur.Model = "stub-1"
	d2, err := s.Draft(ctx, owner, cur, "add: close at six")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Spec.Model != "stub-1" || len(d2.Spec.Rules) != 2 {
		t.Errorf("second draft: %+v", d2.Spec)
	}
	// And the draft is a spec Create accepts as it stands.
	if _, err := s.Create(ctx, owner, d2.Spec); err != nil {
		t.Errorf("a draft was refused at create: %v", err)
	}

	for _, bad := range []string{"", strings.Repeat("x", MaxMessageBytes+1)} {
		if _, err := s.Draft(ctx, owner, cur, bad); err == nil {
			t.Errorf("draft of %d bytes was accepted", len(bad))
		}
	}
	if _, err := s.Draft(ctx, Owner{}, cur, "x"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("draft without an owner: %v", err)
	}
}

func TestDraftIsRefusedBeforeTheGrantIsEmpty(t *testing.T) {
	// Enough for a message, not for a draft's hold: the same wallet still
	// answers, and drafts say so without spending.
	s, l, _, owner := newService(t, &proxy.StubProvider{}, 3_000)
	ctx := context.Background()
	before, _ := l.Balance(ctx, owner.Wallet)
	_, err := s.Draft(ctx, owner, steward, "be terse")
	if !errors.Is(err, ErrOutOfCredits) {
		t.Fatalf("draft on a thin grant: %v, want ErrOutOfCredits", err)
	}
	if after, _ := l.Balance(ctx, owner.Wallet); after != before {
		t.Errorf("refused draft moved money: %d -> %d", before, after)
	}
	ag, _ := s.Create(ctx, owner, steward)
	if _, err := s.Say(ctx, ag.ID, "", "hello"); err != nil {
		t.Errorf("the same grant should still answer a message: %v", err)
	}
}

func TestDraftOverHTTP(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 1_000_000)
	control := httptest.NewServer(s.Control(bearer(owner)))
	defer control.Close()
	code, out := as(t, owner, "POST", control.URL+"/v1/draft", `{"spec":{"version":1},"request":"a poet"}`)
	if code != 200 {
		t.Fatalf("draft: %d %v", code, out)
	}
	if out["spec"].(map[string]any)["name"] == "" || out["cost"].(float64) <= 0 {
		t.Errorf("draft over HTTP: %v", out)
	}
	if code, _ := as(t, owner, "POST", control.URL+"/v1/draft", `{"spec":{"colour":"red"},"request":"x"}`); code != 400 {
		t.Errorf("draft with an unknown spec key: %d, want 400", code)
	}
	if code, _ := as(t, Owner{}, "POST", control.URL+"/v1/draft", `{"request":"x"}`); code != 401 {
		t.Errorf("signed-out draft: %d, want 401", code)
	}
	s.cfg.BuilderModel = ""
	if code, _ := as(t, owner, "POST", control.URL+"/v1/draft", `{"request":"x"}`); code != 503 {
		t.Errorf("draft with no builder model: %d, want 503", code)
	}
}
