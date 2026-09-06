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
// recently used forgotten first, and end with the process. A built agent has
// no memory across conversations yet; that is a later addition, and so are
// tools.
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
	ErrNoAgent         = errors.New("service: no such agent")
	ErrNoConversation  = errors.New("service: no such conversation")
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
	Log    *slog.Logger
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

// Turn is one exchange: what the agent said, and what it cost.
type Turn struct {
	Conversation string         `json:"conversation"`
	Reply        string         `json:"reply"`
	Cost         ledger.Credits `json:"cost"`
	Balance      ledger.Credits `json:"balance"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	now    func() time.Time

	mu     sync.Mutex
	agents map[string]*Agent
	convs  map[string]*conversation
}

func New(cfg Config) *Service {
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
	return &Service{
		cfg:    cfg,
		log:    log,
		client: &http.Client{Transport: inProcess{cfg.Proxy}},
		now:    now,
		agents: map[string]*Agent{},
		convs:  map[string]*conversation{},
	}
}

// Create validates a spec and starts running it on its owner's wallet. The
// model is checked against the price table here, so a spec naming a model
// the platform does not offer is refused at creation, not on its first
// message. The wallet must already exist: the service mints nothing.
func (s *Service) Create(ctx context.Context, owner Owner, a spec.Agent) (*Agent, error) {
	if owner.ID == "" || owner.Wallet == "" {
		return nil, errors.New("service: an agent needs an owner with a wallet")
	}
	if a.MaxReplyTokens == 0 {
		a.MaxReplyTokens = spec.DefaultMaxReplyTokens
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if _, ok := s.cfg.Proxy.Table.Lookup(a.Model); !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotOffered, a.Model)
	}
	if _, err := s.cfg.Ledger.Get(ctx, owner.Wallet); err != nil {
		return nil, fmt.Errorf("owner's wallet: %w", err)
	}

	now := s.now()
	ag := &Agent{
		ID: "a_" + randomHex(8), Owner: owner.ID, Spec: a, Wallet: owner.Wallet, Created: now,
		token: randomHex(32), tokens: float64(s.cfg.Rate.Burst), filled: now,
		convs: map[string]*conversation{},
	}
	s.cfg.Proxy.Authorize(ag.token, owner.Wallet)

	s.mu.Lock()
	s.agents[ag.ID] = ag
	s.mu.Unlock()
	s.log.Info("agent created", "agent", ag.ID, "name", a.Name, "model", a.Model, "owner", owner.ID)
	return ag, nil
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
	s.mu.Unlock()

	// One call on a conversation at a time. A second message that arrives
	// while the first is being answered waits, then sees the answer.
	conv.mu.Lock()
	defer conv.mu.Unlock()
	history := append(append([]message(nil), conv.messages...), message{"user", text})
	reply, cost, balance, err := s.complete(ctx, ag, history)
	if err != nil {
		return Turn{}, err
	}
	conv.messages = append(history, message{"assistant", reply})
	if keep := s.cfg.History * 2; len(conv.messages) > keep {
		conv.messages = conv.messages[len(conv.messages)-keep:]
	}
	return Turn{Conversation: convID, Reply: reply, Cost: cost, Balance: balance}, nil
}

// complete makes the one metered call. It is the Python SDK's Model.complete
// in Go: the same body, the same bearer token, the same three refusals read
// off the same status codes.
func (s *Service) complete(ctx context.Context, ag *Agent, history []message) (string, ledger.Credits, ledger.Credits, error) {
	body, err := json.Marshal(map[string]any{
		"model":      ag.Spec.Model,
		"max_tokens": ag.Spec.MaxReplyTokens,
		"system":     spec.Prompt(ag.Spec),
		"messages":   history,
	})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+ag.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("proxy: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", 0, 0, fmt.Errorf("read proxy response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusPaymentRequired:
		return "", 0, 0, ErrOutOfCredits
	case http.StatusBadGateway:
		return "", 0, 0, ErrProviderDown
	case http.StatusForbidden:
		return "", 0, 0, ErrModelNotOffered
	default:
		return "", 0, 0, fmt.Errorf("proxy returned %d: %.200s", resp.StatusCode, out)
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", 0, 0, fmt.Errorf("model response: %w", err)
	}
	var reply strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			reply.WriteString(c.Text)
		}
	}
	cost, _ := strconv.ParseInt(resp.Header.Get("X-Phylum-Cost"), 10, 64)
	balance, _ := strconv.ParseInt(resp.Header.Get("X-Phylum-Balance"), 10, 64)
	return strings.TrimSpace(reply.String()), ledger.Credits(cost), ledger.Credits(balance), nil
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
