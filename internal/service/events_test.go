package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/spec"
)

func withWebhook(instruction string) spec.Agent {
	a := steward
	a.Webhook = &spec.Webhook{Instruction: instruction}
	return a
}

// An event is a message from the owner's side: the agent answers it as
// itself, about what the event said, on the owner's wallet, on one thread
// all events share; and an agent without a webhook has no such endpoint.
func TestAnEventWakesTheAgentAndAMessageCouldNot(t *testing.T) {
	s, _, rec, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	ctx := context.Background()
	hooked, err := s.Create(ctx, owner, withWebhook("An order came in. Thank the customer by name."))
	if err != nil {
		t.Fatal(err)
	}
	event := []byte(`{"customer":"Ada","tea":"chamomile-extraordinaire"}`)
	turn, err := s.Event(ctx, hooked.ID, event)
	if err != nil {
		t.Fatal(err)
	}
	// The stub reads the longest word of what it was given, and that word
	// is in the event, not the instruction: the agent heard the event.
	if !strings.Contains(turn.Reply, "extraordinaire") || !strings.HasPrefix(turn.Reply, steward.Name) {
		t.Errorf("reply = %q, want %s answering about the event", turn.Reply, steward.Name)
	}
	if turn.Cost <= 0 || rec.last(t).Wallet != hooked.Wallet {
		t.Errorf("turn = %+v, event = %+v; want a charge on the agent's wallet", turn, rec.last(t))
	}
	// Events share one standing thread, and a person cannot speak into it.
	again, err := s.Event(ctx, hooked.ID, []byte(`{"customer":"Bob","tea":"assam"}`))
	if err != nil || again.Conversation != turn.Conversation {
		t.Errorf("second event: %v, conversation %q, want the first's %q", err, again.Conversation, turn.Conversation)
	}
	if thread := s.agents[hooked.ID].events.messages; len(thread) != 4 || thread[0].Content != string(event) {
		t.Errorf("event thread = %+v, want both events verbatim with their replies", thread)
	}
	if _, err := s.Say(ctx, hooked.ID, turn.Conversation, "and what did Ada order?"); !errors.Is(err, ErrNoConversation) {
		t.Errorf("a person speaking into the event thread: %v, want ErrNoConversation", err)
	}

	plain, _ := s.Create(ctx, owner, steward)
	if _, err := s.Event(ctx, plain.ID, event); !errors.Is(err, ErrNoWebhook) {
		t.Errorf("event to an agent without a webhook: %v, want ErrNoWebhook", err)
	}
	if _, err := s.Event(ctx, hooked.ID, []byte(" \n")); !errors.Is(err, ErrEmptyMessage) {
		t.Errorf("empty event: %v, want ErrEmptyMessage", err)
	}
	if _, err := s.Event(ctx, hooked.ID, []byte(strings.Repeat("x", MaxMessageBytes+1))); !errors.Is(err, ErrMessageTooLong) {
		t.Errorf("oversized event: %v, want ErrMessageTooLong", err)
	}
}

// The endpoint, over HTTP: any body, a reply, and a 404 where there is no
// webhook — the same 404 as an agent that does not exist, so the surface
// does not say which.
func TestTheWebhookIsAPublicPostOfAnyBody(t *testing.T) {
	s, _, _, owner := newService(t, &proxy.StubProvider{}, 100_000_000)
	ctx := context.Background()
	hooked, _ := s.Create(ctx, owner, withWebhook("A build finished. Say whether it passed."))
	plain, _ := s.Create(ctx, owner, steward)
	public := httptest.NewServer(s.Public())
	defer public.Close()

	resp, err := http.Post(public.URL+"/a/"+hooked.ID+"/events", "text/plain", strings.NewReader("build 41 passed, 3 deprecations"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Conversation, Reply string }
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out.Conversation == "" || !strings.Contains(out.Reply, "deprecations") ||
		resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("event: %d %+v %v", resp.StatusCode, out, resp.Header)
	}

	for name, path := range map[string]string{"no webhook": "/a/" + plain.ID + "/events", "no agent": "/a/a_nobody/events"} {
		resp, _ := http.Post(public.URL+path, "application/json", strings.NewReader(`{}`))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, resp.StatusCode)
		}
	}
	resp, _ = http.Post(public.URL+"/a/"+hooked.ID+"/events", "text/plain", strings.NewReader(strings.Repeat("x", MaxMessageBytes+1)))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized event: %d, want 400", resp.StatusCode)
	}
}
