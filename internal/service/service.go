// Package service is the runtime for built agents: a message comes in over
// HTTP, the agent's spec becomes one metered model call through the proxy,
// and the reply goes back out.
//
// It is the proxy's second caller. The arena's agents reach the proxy from
// inside a container with a per-container token; a service agent reaches it
// from this process with a per-agent token, over an in-process round-tripper
// rather than a socket. Everything else is the same arc — price the worst
// case, hold it, invoke, settle, record — and that sameness is the point: the
// hard cap on what an agent can spend is enforced by the hold, not by any
// code here, and a service agent that runs dry is refused by the proxy exactly
// as a bankrupt bidder is.
//
// Two HTTP surfaces, kept apart from the start. Control creates agents and
// lists them; it belongs to whoever is signed in, and shows each person only
// their own. Public is the endpoint each agent is reachable on, and it is
// meant for strangers: it names the agent, hands out its greeting, and takes
// messages, at a rate. Nothing on the public surface can create, fund or
// inspect an agent, so that the widget, when it arrives, attaches to Public
// alone without a refactor.
//
// Every model call is recorded by the proxy's Recorder, request and response
// verbatim, as every arena call is. For a service agent that record is the
// conversation strangers had with it, word for word. It is an on-disk audit
// record for the operator and not a publishable artefact: the arena's traces
// are public by design, a service trace must not be treated the same way.
//
// Agents belong to people. An agent's wallet is its owner's wallet: one grant
// per person, and every agent they build draws on it, so two agents of one
// user run dry together. The proxy does not know this — it sees a token and
// the wallet behind it, as it always has — which is why the cap on a person
// costs the proxy nothing to enforce.
//
// The public endpoint is the one place a stranger can make an owner spend,
// so it is the one place with a rate limit: a token bucket per agent, sized
// so a conversation feels unthrottled and a loop does not empty a dollar in
// a minute. Conversations live in memory, capped per agent with the least
// recently used forgotten first, and end with the process. Agents do not:
// a Store keeps them, and a restart brings each one back under a fresh
// proxy token, with its conversations gone. A built agent has no memory
// across conversations yet; that is a later addition.
//
// A tool is the owner's own HTTP endpoint, described in the spec, and the
// one thing an agent does besides ask the model. The model asks for it by
// name in a reply; the service makes the request itself, with the guard in
// egress.go on the address, and hands back what came out as the next thing
// the model reads. One message may go round this a few times before the
// model answers in words, and every round is a metered call like the first,
// held and settled by the proxy one at a time — so a message with tools in
// it may cost several holds from one bucket token, and the ceiling on
// rounds is what bounds it.
//
// Draft is the other way of building. A person describes a change in plain
// language, the current spec and the request go to the builder model, and a
// revised spec comes back for the person to keep or not. It is a metered
// call like any other, on the owner's own grant: the platform does not pay
// for the thinking, and a dollar buys a fixed amount of it. It buys less
// than it buys of replies — a draft may write the whole spec back, so its
// ceiling, and the hold behind it, is many times a reply's.
package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/spec"
)

var (
	ErrNoAgent        = errors.New("service: no such agent")
	ErrNoConversation = errors.New("service: no such conversation")
	// ErrNoWebhook is an event for an agent whose spec has no webhook. It
	// answers as a 404, like an agent that is not there: the endpoint does
	// not exist until the owner opens it.
	ErrNoWebhook       = errors.New("service: agent has no webhook")
	ErrModelNotOffered = errors.New("service: model is not offered")
	// ErrOutOfCredits is the proxy's 402, said the service's way. It is the
	// one refusal a person talking to an agent will meet on purpose: the
	// owner's grant is spent, and nothing more was charged.
	ErrOutOfCredits = errors.New("service: agent is out of credits")
	// ErrProviderDown is the proxy's 502. Nothing was charged.
	ErrProviderDown   = errors.New("service: model unavailable")
	ErrEmptyMessage   = errors.New("service: message is empty")
	ErrMessageTooLong = errors.New("service: message is too long")
	// ErrRateLimited is the public endpoint's 429: this agent is being
	// messaged faster than its bucket refills. Nothing was charged. The error
	// returned is a *RateLimitError, which says how long to wait.
	ErrRateLimited = errors.New("service: too many messages")
	// ErrUnauthenticated is the control surface's 401: no session, or one
	// that has ended.
	ErrUnauthenticated = errors.New("service: not signed in")
	// ErrBadDraft is the builder model's failure: it answered, and what it
	// answered was not a spec this platform reads. The call was charged, as a
	// wrong answer from any model is.
	ErrBadDraft = errors.New("service: the builder's draft is not a valid spec")
	// ErrNoBuilder is a Draft with no builder model configured.
	ErrNoBuilder = errors.New("service: no builder model")
)

// RateLimitError carries the wait. errors.Is(err, ErrRateLimited) holds.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%v; retry in %s", ErrRateLimited, e.RetryAfter.Round(time.Second))
}
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// MaxMessageBytes bounds one incoming message. It bounds the worst case the
// proxy has to hold for it too, since input is priced by the byte.
const MaxMessageBytes = 4096

// DefaultHistory is how many exchanges (one message and its reply) a
// conversation carries into the next call. Every one of them is paid for
// again on every call, so the window is short.
const DefaultHistory = 12

// DefaultConversations is how many open conversations one agent keeps before
// the least recently used is forgotten to make room.
const DefaultConversations = 1000

// DraftMaxTokens is the reply ceiling of a chat-to-spec call. A draft carries
// the whole revised spec, and a spec at its limits is several thousand
// tokens, so the ceiling is the spec's own ceiling. The proxy holds this
// much before every draft: on the launch model that is a few cents, roughly
// ten replies' worth, and a grant with less than that left is refused a
// draft while it can still answer messages.
const DraftMaxTokens = spec.MaxReplyTokensCeiling

// Rate is the public endpoint's token bucket, per agent. Burst is how many
// messages an agent answers back-to-back from a standing start; PerMinute is
// the steady rate once the burst is spent.
//
// The two numbers trade a stranger's experience against how fast one stranger
// can spend the owner's dollar. A person in a chat sends a message every ten
// seconds or so, and never ten in a row; a script does. At the default a
// script can make an agent answer at most twenty times a minute, which on the
// launch model and a full history window is a few cents — slow enough that an
// owner sees the balance move before it is gone.
type Rate struct {
	Burst     int
	PerMinute int
}

// DefaultRate is the rate an agent gets when its Config names none.
var DefaultRate = Rate{Burst: 10, PerMinute: 20}

// Config assembles a Service. Proxy and Ledger are required.
type Config struct {
	Proxy  *proxy.Proxy
	Ledger *ledger.Ledger
	// History is the number of exchanges kept per conversation; zero means
	// DefaultHistory.
	History int
	// Conversations caps the open conversations per agent; zero means
	// DefaultConversations.
	Conversations int
	// Rate is the public endpoint's limit per agent; a zero Rate means
	// DefaultRate.
	Rate Rate
	// Locked are the models the catalogue shows and does not offer. Naming
	// one in a spec is refused like any model off the price table; the
	// builder shows them with a lock, and the lock joins the waitlist.
	Locked []string
	// Store keeps agents across restarts; nil keeps them in memory only.
	Store Store
	// BuilderModel is the model Draft calls, on the owner's wallet. It must
	// be on the price table. Empty refuses drafts with ErrNoBuilder.
	BuilderModel string
	// InsecureTools lets a tool reach http and private addresses. For a
	// developer's own machine and never for a deployment: with it set a
	// stranger can make an agent call anything the platform can see.
	InsecureTools bool
	Log           *slog.Logger
	// Now is the clock the rate limit and the conversation cap read; nil
	// means time.Now.
	Now func() time.Time
}

// Owner is who an agent belongs to and whose wallet it spends: a signed-in
// user, or the operator for agents loaded at boot.
type Owner struct {
	ID     string
	Wallet string
}

// Agent is a built agent the service is running.
type Agent struct {
	ID      string     `json:"id"`
	Owner   string     `json:"owner"`
	Spec    spec.Agent `json:"spec"`
	Wallet  string     `json:"wallet"`
	Created time.Time  `json:"created"`
	token   string

	// The bucket and the agent's own conversations, guarded by Service.mu.
	tokens float64
	filled time.Time
	convs  map[string]*conversation
}

// Draft is one chat-to-spec exchange: the revised spec, the builder's note
// on what it changed, and what the call cost the owner.
type Draft struct {
	Spec    spec.Agent     `json:"spec"`
	Note    string         `json:"note"`
	Cost    ledger.Credits `json:"cost"`
	Balance ledger.Credits `json:"balance"`
}

// Turn is one exchange: what the agent said, and what it cost.
type Turn struct {
	Conversation string         `json:"conversation"`
	Reply        string         `json:"reply"`
	Cost         ledger.Credits `json:"cost"`
	Balance      ledger.Credits `json:"balance"`
}

// message is one turn as the model sees it. Content is a string in the
// history a conversation keeps, and a list of blocks in the rounds a tool
// call adds within one message: the model's reply with its tool_use blocks,
// echoed back, and the results as tool_result blocks.
type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// block is one content block, either way: text or tool_use from the model,
// tool_result to it.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// conversation is one thread with one agent. Its mutex serialises the calls
// made on it, so two messages arriving together are answered one after the
// other and each sees the one before; Service.mu guards the rest.
type conversation struct {
	agent    string
	mu       sync.Mutex
	messages []message
	last     time.Time
}

// Service runs built agents. It is safe for concurrent use.
type Service struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client
	egress *http.Client
	now    func() time.Time

	mu     sync.Mutex
	agents map[string]*Agent
	convs  map[string]*conversation
	// builders are the per-owner proxy tokens drafts are charged on, made
	// on first use. An agent's token spends the same wallet; a draft has no
	// agent yet, so the owner gets a token of their own.
	builders map[string]string
}

// New assembles the service and brings back every agent in its Store, each
// under a fresh proxy token. A stored agent is not re-validated: the limits
// are for what is written, and what was written stands.
func New(cfg Config) (*Service, error) {
	if cfg.History <= 0 {
		cfg.History = DefaultHistory
	}
	if cfg.Conversations <= 0 {
		cfg.Conversations = DefaultConversations
	}
	if cfg.Rate == (Rate{}) {
		cfg.Rate = DefaultRate
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		cfg:      cfg,
		log:      log,
		client:   &http.Client{Transport: inProcess{cfg.Proxy}},
		egress:   egressClient(cfg.InsecureTools),
		now:      now,
		agents:   map[string]*Agent{},
		convs:    map[string]*conversation{},
		builders: map[string]string{},
	}
	if cfg.Store == nil {
		return s, nil
	}
	records, err := cfg.Store.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("service: load agents: %w", err)
	}
	for _, r := range records {
		if r.Spec.MaxReplyTokens == 0 {
			r.Spec.MaxReplyTokens = spec.DefaultMaxReplyTokens
		}
		ag := s.run(r.ID, r.Owner, r.Wallet, r.Spec, r.Created)
		s.agents[ag.ID] = ag
		if _, ok := cfg.Proxy.Table.Lookup(r.Spec.Model); !ok {
			log.Warn("agent's model is not offered; its messages will be refused", "agent", ag.ID, "model", r.Spec.Model)
		}
	}
	log.Info("agents loaded", "count", len(records))
	return s, nil
}

// run makes the in-memory agent for a spec — a token the proxy will honour
// and a full bucket — and authorises it. Called for a new agent and for a
// stored one alike; a revised agent is run again from its old token.
func (s *Service) run(id, owner, wallet string, a spec.Agent, created time.Time) *Agent {
	ag := &Agent{
		ID: id, Owner: owner, Spec: a, Wallet: wallet, Created: created,
		token: randomHex(32), tokens: float64(s.cfg.Rate.Burst), filled: s.now(),
		convs: map[string]*conversation{},
	}
	s.cfg.Proxy.Authorize(ag.token, wallet)
	return ag
}

// check is what Create and Update both ask of a spec: that it validates,
// names a model on the table, and gives its tools URLs the platform will
// call.
func (s *Service) check(a *spec.Agent) error {
	if a.MaxReplyTokens == 0 {
		a.MaxReplyTokens = spec.DefaultMaxReplyTokens
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if _, ok := s.cfg.Proxy.Table.Lookup(a.Model); !ok {
		return fmt.Errorf("%w: %s", ErrModelNotOffered, a.Model)
	}
	for _, t := range a.Tools {
		if err := checkTool(t, s.cfg.InsecureTools); err != nil {
			return err
		}
	}
	return nil
}

// save writes an agent to the store, if there is one.
func (s *Service) save(ctx context.Context, ag *Agent) error {
	if s.cfg.Store == nil {
		return nil
	}
	r := Record{ID: ag.ID, Owner: ag.Owner, Wallet: ag.Wallet, Spec: ag.Spec, Created: ag.Created}
	if err := s.cfg.Store.Put(ctx, r); err != nil {
		s.log.Error("store put failed", "agent", ag.ID, "err", err)
		return errStore
	}
	return nil
}

// Create validates a spec and starts running it on its owner's wallet. The
// model is checked against the price table here, so a spec naming a model
// the platform does not offer is refused at creation, not on its first
// message. The wallet must already exist: the service mints nothing.
func (s *Service) Create(ctx context.Context, owner Owner, a spec.Agent) (*Agent, error) {
	if owner.ID == "" || owner.Wallet == "" {
		return nil, errors.New("service: an agent needs an owner with a wallet")
	}
	if err := s.check(&a); err != nil {
		return nil, err
	}
	if _, err := s.cfg.Ledger.Get(ctx, owner.Wallet); err != nil {
		return nil, fmt.Errorf("owner's wallet: %w", err)
	}

	ag := s.run("a_"+randomHex(8), owner.ID, owner.Wallet, a, s.now())
	if err := s.save(ctx, ag); err != nil {
		s.cfg.Proxy.Revoke(ag.token)
		return nil, err
	}
	s.mu.Lock()
	s.agents[ag.ID] = ag
	s.mu.Unlock()
	s.log.Info("agent created", "agent", ag.ID, "name", a.Name, "model", a.Model, "owner", owner.ID)
	return ag, nil
}

// Update replaces an agent's spec, checked as a new one is. Its
// conversations end with it: the history was with the agent it used to be.
// The agent's token and its bucket carry over, so an edit is not a way to
// refill the bucket, and a message in flight on the old spec finishes on
// the old spec — the agent it was answering as is the one the caller heard.
func (s *Service) Update(ctx context.Context, id string, a spec.Agent) (*Agent, error) {
	if err := s.check(&a); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.agents[id]
	if !ok {
		return nil, ErrNoAgent
	}
	// A new value rather than a write to the old one: pointers handed out
	// by Get and AgentsOf are being encoded outside this lock, and they
	// must go on reading the spec they were given.
	ag := &Agent{
		ID: old.ID, Owner: old.Owner, Spec: a, Wallet: old.Wallet, Created: old.Created,
		token: old.token, tokens: old.tokens, filled: old.filled,
		convs: map[string]*conversation{},
	}
	if err := s.save(ctx, ag); err != nil {
		return nil, err
	}
	for cid := range old.convs {
		delete(s.convs, cid)
	}
	s.agents[id] = ag
	s.log.Info("agent updated", "agent", id, "name", a.Name, "model", a.Model)
	return ag, nil
}

// Delete stops an agent: its token is revoked at the proxy, its
// conversations are forgotten, and its endpoint answers 404 from now on. A
// message in flight completes — the hold was taken — and is the last.
func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ag, ok := s.agents[id]
	if !ok {
		return ErrNoAgent
	}
	if s.cfg.Store != nil {
		if err := s.cfg.Store.Delete(ctx, id); err != nil {
			s.log.Error("store delete failed", "agent", id, "err", err)
			return errStore
		}
	}
	for cid := range ag.convs {
		delete(s.convs, cid)
	}
	delete(s.agents, id)
	s.cfg.Proxy.Revoke(ag.token)
	s.log.Info("agent deleted", "agent", id, "name", ag.Spec.Name)
	return nil
}

// Get returns a running agent.
func (s *Service) Get(id string) (*Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	return a, ok
}

// Agents lists the running agents, oldest first.
func (s *Service) Agents() []*Agent { return s.AgentsOf("") }

// AgentsOf lists one owner's agents, oldest first; an empty owner lists all.
func (s *Service) AgentsOf(owner string) []*Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		if owner == "" || a.Owner == owner {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Models is the catalogue: what a spec may name, and what it may not yet.
func (s *Service) Models() (offered, locked []string) {
	return s.cfg.Proxy.Table.Models(), append([]string(nil), s.cfg.Locked...)
}

// take spends one token from the agent's bucket, or says how long until
// there is one. Called with s.mu held.
func (s *Service) take(ag *Agent, now time.Time) (time.Duration, bool) {
	perSec := float64(s.cfg.Rate.PerMinute) / 60
	ag.tokens = min(float64(s.cfg.Rate.Burst), ag.tokens+now.Sub(ag.filled).Seconds()*perSec)
	ag.filled = now
	if ag.tokens >= 1 {
		ag.tokens--
		return 0, true
	}
	if perSec <= 0 {
		return time.Hour, false
	}
	return time.Duration((1 - ag.tokens) / perSec * float64(time.Second)), false
}

// open starts a conversation for an agent, forgetting its least recently
// used one if the agent is at its cap. Called with s.mu held.
func (s *Service) open(ag *Agent, now time.Time) (string, *conversation) {
	if len(ag.convs) >= s.cfg.Conversations {
		var oldest string
		for id, c := range ag.convs {
			if oldest == "" || c.last.Before(ag.convs[oldest].last) {
				oldest = id
			}
		}
		delete(ag.convs, oldest)
		delete(s.convs, oldest)
	}
	id := "c_" + randomHex(16)
	conv := &conversation{agent: ag.ID, last: now}
	ag.convs[id] = conv
	s.convs[id] = conv
	return id, conv
}

// Say delivers one message to an agent and returns its reply. An empty
// conversation id opens a new conversation; the id in the returned Turn
// continues it. A conversation belongs to the agent it was opened with.
//
// The message joins the conversation only once it has been answered: a
// refused call leaves the history as it was, so a person can try again later
// without the agent having half-heard them.
func (s *Service) Say(ctx context.Context, agentID, convID, text string) (Turn, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return Turn{}, ErrEmptyMessage
	case len(text) > MaxMessageBytes:
		return Turn{}, ErrMessageTooLong
	}
	return s.say(ctx, agentID, convID, text)
}

// Event delivers an inbound event to an agent whose spec has a webhook and
// returns what the agent made of it. An event is a message the owner's
// systems send instead of a person: it opens a conversation of its own,
// is rate-limited on the same bucket, and costs the owner's wallet what a
// message does. The body is bounded like a message; the instruction it is
// framed with is bounded by the spec, so the two together are still one
// short call.
func (s *Service) Event(ctx context.Context, agentID string, body []byte) (Turn, error) {
	body = bytes.TrimSpace(body)
	switch {
	case len(body) == 0:
		return Turn{}, ErrEmptyMessage
	case len(body) > MaxMessageBytes:
		return Turn{}, ErrMessageTooLong
	}
	s.mu.Lock()
	ag, ok := s.agents[agentID]
	var hook *spec.Webhook
	if ok {
		hook = ag.Spec.Webhook
	}
	s.mu.Unlock()
	switch {
	case !ok:
		return Turn{}, ErrNoAgent
	case hook == nil:
		return Turn{}, ErrNoWebhook
	}
	return s.say(ctx, agentID, "", frameEvent(hook.Instruction, body))
}

// frameEvent is how an event reaches the model: as one message from the
// owner's side, the instruction first and the event after it, so the model
// reads what to do before it reads what happened. The event goes in as it
// came — JSON stays JSON — since the model reads it better than any
// flattening would, and the instruction is the owner's to word.
func frameEvent(instruction string, body []byte) string {
	return strings.TrimSpace(instruction) + "\n\nEvent:\n" + string(body)
}

// say is Say once its text has been checked. Called by Say and by Event,
// whose text is an instruction and an event together.
func (s *Service) say(ctx context.Context, agentID, convID, text string) (Turn, error) {
	now := s.now()
	s.mu.Lock()
	ag, ok := s.agents[agentID]
	if !ok {
		s.mu.Unlock()
		return Turn{}, ErrNoAgent
	}
	// The bucket is checked before anything else the message could touch:
	// a flood of bad conversation ids is still a flood.
	if wait, ok := s.take(ag, now); !ok {
		s.mu.Unlock()
		return Turn{}, &RateLimitError{RetryAfter: wait}
	}
	var conv *conversation
	if convID == "" {
		convID, conv = s.open(ag, now)
	} else if conv, ok = s.convs[convID]; !ok || conv.agent != agentID {
		s.mu.Unlock()
		return Turn{}, ErrNoConversation
	}
	conv.last = now
	// What the call needs is copied out under the lock: an Update landing
	// mid-call swaps the agent, and this message is answered by the agent
	// it was sent to.
	c := call{model: ag.Spec.Model, maxTokens: ag.Spec.MaxReplyTokens, system: spec.Prompt(ag.Spec), token: ag.token,
		agent: ag.ID, tools: ag.Spec.Tools}
	s.mu.Unlock()

	// One call on a conversation at a time. A second message that arrives
	// while the first is being answered waits, then sees the answer.
	conv.mu.Lock()
	defer conv.mu.Unlock()
	history := append(append([]message(nil), conv.messages...), message{"user", text})
	reply, cost, balance, err := s.converse(ctx, c, history)
	if err != nil {
		return Turn{}, err
	}
	conv.messages = append(history, message{"assistant", reply})
	if keep := s.cfg.History * 2; len(conv.messages) > keep {
		conv.messages = conv.messages[len(conv.messages)-keep:]
	}
	return Turn{Conversation: convID, Reply: reply, Cost: cost, Balance: balance}, nil
}

// Draft asks the builder model to revise a spec as a person described, and
// charges the owner for it. The current spec may be incomplete — a new
// agent has no name yet — and goes to the model as it is; the model's
// answer is a draft until the service has pinned what the model may not
// choose (the version, and the model the agent runs on) and found it valid.
// The draft is returned, not kept: keeping it is a Create or an Update,
// which check it again.
func (s *Service) Draft(ctx context.Context, owner Owner, current spec.Agent, request string) (Draft, error) {
	request = strings.TrimSpace(request)
	switch {
	case s.cfg.BuilderModel == "":
		return Draft{}, ErrNoBuilder
	case owner.ID == "" || owner.Wallet == "":
		return Draft{}, ErrUnauthenticated
	case request == "":
		return Draft{}, ErrEmptyMessage
	case len(request) > MaxMessageBytes:
		return Draft{}, ErrMessageTooLong
	}
	c := call{model: s.cfg.BuilderModel, maxTokens: DraftMaxTokens, system: spec.BuilderPrompt(current), token: s.builder(owner)}
	out, err := s.complete(ctx, c, []message{{"user", request}}, false)
	if err != nil {
		return Draft{}, err
	}
	reply, cost, balance := out.text, out.cost, out.balance
	d, err := spec.ParseDraft(reply)
	if err != nil {
		s.log.Warn("builder reply was not a draft", "owner", owner.ID, "err", err, "reply", fmt.Sprintf("%.200s", reply))
		return Draft{}, fmt.Errorf("%w: %v", ErrBadDraft, err)
	}
	d.Spec.Version = spec.Version
	d.Spec.Model = current.Model
	if d.Spec.Model == "" {
		d.Spec.Model = s.cfg.BuilderModel
	}
	if d.Spec.MaxReplyTokens == 0 {
		d.Spec.MaxReplyTokens = spec.DefaultMaxReplyTokens
	}
	if err := d.Spec.Validate(); err != nil {
		return Draft{}, fmt.Errorf("%w: %v", ErrBadDraft, err)
	}
	return Draft{Spec: d.Spec, Note: d.Note, Cost: cost, Balance: balance}, nil
}

// builder is the owner's proxy token for drafts, made and authorised on
// first use.
func (s *Service) builder(owner Owner) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.builders[owner.ID]
	if !ok {
		tok = randomHex(32)
		s.cfg.Proxy.Authorize(tok, owner.Wallet)
		s.builders[owner.ID] = tok
	}
	return tok
}

// call is one metered call's fixed part: the model and ceiling, the system
// prompt, the token that pays, and for an agent's message, the agent and
// the tools it may ask for.
type call struct {
	model     string
	maxTokens int64
	system    string
	token     string
	agent     string
	tools     []spec.Tool
}

// completion is what one metered call came back with: the words, the whole
// content as sent (to echo back if a tool was asked for), the tool_use
// blocks in it, and what it cost.
type completion struct {
	text    string
	content json.RawMessage
	uses    []block
	cost    ledger.Credits
	balance ledger.Credits
}

// converse answers one message: a call, and if the model asked for tools,
// their results and another call, up to MaxToolRounds times. On the last
// round the model is told it may not ask again, so the message ends in
// words. Costs add up across rounds; the balance is the last one seen. A
// round that is refused ends the message with nothing said, though the
// rounds before it were charged — the proxy recorded each.
func (s *Service) converse(ctx context.Context, c call, msgs []message) (string, ledger.Credits, ledger.Credits, error) {
	ctx, cancel := context.WithTimeout(ctx, MessageDeadline)
	defer cancel()
	var total ledger.Credits
	for round := 0; ; round++ {
		last := round >= MaxToolRounds
		out, err := s.complete(ctx, c, msgs, last)
		if err != nil {
			return "", total, 0, err
		}
		total += out.cost
		if len(out.uses) == 0 || last {
			reply := out.text
			if reply == "" {
				// A model that stops with nothing to say would leave an
				// empty turn in the history, which the next call refuses.
				reply = "…"
			}
			return reply, total, out.balance, nil
		}
		results := make([]block, 0, len(out.uses))
		for _, u := range out.uses {
			var tool spec.Tool
			for _, t := range c.tools {
				if t.Name == u.Name {
					tool = t
				}
			}
			text, failed := "there is no tool called "+u.Name, true
			if tool.Name != "" {
				text, failed = s.callTool(ctx, c.agent, tool, u.Input)
			}
			results = append(results, block{Type: "tool_result", ToolUseID: u.ID, Content: text, IsError: failed})
		}
		msgs = append(msgs, message{"assistant", out.content}, message{"user", results})
	}
}

// complete makes the one metered call. It is the Python SDK's Model.complete
// in Go: the same body, the same bearer token, the same three refusals read
// off the same status codes. With tools on the call they go along in the
// provider's shape, every parameter a string; noTools keeps them defined
// (the history may refer to them) but tells the model not to ask.
func (s *Service) complete(ctx context.Context, c call, history []message, noTools bool) (completion, error) {
	payload := map[string]any{
		"model":      c.model,
		"max_tokens": c.maxTokens,
		"system":     c.system,
		"messages":   history,
	}
	if len(c.tools) > 0 {
		tools := make([]map[string]any, 0, len(c.tools))
		for _, t := range c.tools {
			props := map[string]any{}
			for _, p := range t.Params {
				props[p.Name] = map[string]any{"type": "string", "description": p.Description}
			}
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description,
				"input_schema": map[string]any{"type": "object", "properties": props},
			})
		}
		payload["tools"] = tools
		if noTools {
			payload["tool_choice"] = map[string]any{"type": "none"}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return completion{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/v1/messages", bytes.NewReader(body))
	if err != nil {
		return completion{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return completion{}, fmt.Errorf("proxy: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return completion{}, fmt.Errorf("read proxy response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusPaymentRequired:
		return completion{}, ErrOutOfCredits
	case http.StatusBadGateway:
		return completion{}, ErrProviderDown
	case http.StatusForbidden:
		return completion{}, ErrModelNotOffered
	default:
		return completion{}, fmt.Errorf("proxy returned %d: %.200s", resp.StatusCode, out)
	}

	var parsed struct {
		Content json.RawMessage `json:"content"`
	}
	var blocks []block
	if err := json.Unmarshal(out, &parsed); err != nil {
		return completion{}, fmt.Errorf("model response: %w", err)
	}
	if err := json.Unmarshal(parsed.Content, &blocks); err != nil {
		return completion{}, fmt.Errorf("model response content: %w", err)
	}
	var reply strings.Builder
	var uses []block
	for _, b := range blocks {
		switch b.Type {
		case "text":
			reply.WriteString(b.Text)
		case "tool_use":
			uses = append(uses, b)
		}
	}
	cost, _ := strconv.ParseInt(resp.Header.Get("X-Phylum-Cost"), 10, 64)
	balance, _ := strconv.ParseInt(resp.Header.Get("X-Phylum-Balance"), 10, 64)
	return completion{text: strings.TrimSpace(reply.String()), content: parsed.Content, uses: uses,
		cost: ledger.Credits(cost), balance: ledger.Credits(balance)}, nil
}

// inProcess dispatches a request straight to a handler: the proxy's HTTP
// surface without the socket, so the service goes through every check the
// proxy makes and none it does not.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	t.h.ServeHTTP(rec, r)
	return &http.Response{
		StatusCode: rec.status,
		Header:     rec.header,
		Body:       io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		Request:    r,
	}, nil
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("service: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
