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
// lists them; it is the operator's, and later the builder's. Public is the
// endpoint each agent is reachable on, and it is meant for strangers: it names
// the agent, hands out its greeting, and takes messages. Nothing on the public
// surface can create, fund or inspect an agent, so that when accounts, rate
// limits and the widget arrive they attach to one surface each without a
// refactor.
//
// Every model call is recorded by the proxy's Recorder, request and response
// verbatim, as every arena call is. For a service agent that record is the
// conversation strangers had with it, word for word. It is an on-disk audit
// record for the operator and not a publishable artefact: the arena's traces
// are public by design, a service trace must not be treated the same way.
//
// Conversations live in memory and end with the process. A built agent has
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
	// agent's grant is spent, and nothing more was charged.
	ErrOutOfCredits = errors.New("service: agent is out of credits")
	// ErrProviderDown is the proxy's 502. Nothing was charged.
	ErrProviderDown   = errors.New("service: model unavailable")
	ErrEmptyMessage   = errors.New("service: message is empty")
	ErrMessageTooLong = errors.New("service: message is too long")
)

// MaxMessageBytes bounds one incoming message. It bounds the worst case the
// proxy has to hold for it too, since input is priced by the byte.
const MaxMessageBytes = 4096

// DefaultHistory is how many exchanges (one message and its reply) a
// conversation carries into the next call. Every one of them is paid for
// again on every call, so the window is short.
const DefaultHistory = 12

// Config assembles a Service. Proxy and Ledger are required.
type Config struct {
	Proxy  *proxy.Proxy
	Ledger *ledger.Ledger
	// Grant is minted into every new agent's wallet at creation. It is the
	// whole of what the agent can ever spend until something mints more.
	Grant ledger.Credits
	// History is the number of exchanges kept per conversation; zero means
	// DefaultHistory.
	History int
	Log     *slog.Logger
}

// Agent is a built agent the service is running.
type Agent struct {
	ID      string     `json:"id"`
	Spec    spec.Agent `json:"spec"`
	Wallet  string     `json:"wallet"`
	Created time.Time  `json:"created"`
	token   string
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

type conversation struct {
	agent    string
	messages []message
}

// Service runs built agents. It is safe for concurrent use.
type Service struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	mu     sync.Mutex
	agents map[string]*Agent
	convs  map[string]*conversation
}

func New(cfg Config) *Service {
	if cfg.History <= 0 {
		cfg.History = DefaultHistory
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:    cfg,
		log:    log,
		client: &http.Client{Transport: inProcess{cfg.Proxy}},
		agents: map[string]*Agent{},
		convs:  map[string]*conversation{},
	}
}

// Create validates a spec, funds a wallet for it, and starts running it. The
// model is checked against the price table here, so a spec naming a model the
// platform does not offer is refused before it has a wallet, not on its first
// message.
func (s *Service) Create(ctx context.Context, a spec.Agent) (*Agent, error) {
	if a.MaxReplyTokens == 0 {
		a.MaxReplyTokens = spec.DefaultMaxReplyTokens
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if _, ok := s.cfg.Proxy.Table.Lookup(a.Model); !ok {
		return nil, fmt.Errorf("%w: %s", ErrModelNotOffered, a.Model)
	}

	id := "a_" + randomHex(8)
	wallet := "svc:" + id
	if err := s.cfg.Ledger.CreateAccount(ctx, wallet, ledger.KindExperiment, id); err != nil {
		return nil, fmt.Errorf("create wallet: %w", err)
	}
	if s.cfg.Grant > 0 {
		if _, err := s.cfg.Ledger.Mint(ctx, wallet, s.cfg.Grant, "grant", id); err != nil {
			return nil, fmt.Errorf("fund wallet: %w", err)
		}
	}

	ag := &Agent{ID: id, Spec: a, Wallet: wallet, Created: time.Now(), token: randomHex(32)}
	s.cfg.Proxy.Authorize(ag.token, wallet)

	s.mu.Lock()
	s.agents[id] = ag
	s.mu.Unlock()
	s.log.Info("agent created", "agent", id, "name", a.Name, "model", a.Model, "grant", s.cfg.Grant)
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
func (s *Service) Agents() []*Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
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

	s.mu.Lock()
	ag, ok := s.agents[agentID]
	if !ok {
		s.mu.Unlock()
		return Turn{}, ErrNoAgent
	}
	var conv *conversation
	if convID == "" {
		convID = "c_" + randomHex(16)
		conv = &conversation{agent: agentID}
		s.convs[convID] = conv
	} else if conv, ok = s.convs[convID]; !ok || conv.agent != agentID {
		s.mu.Unlock()
		return Turn{}, ErrNoConversation
	}
	history := append(append([]message(nil), conv.messages...), message{"user", text})
	s.mu.Unlock()

	reply, cost, balance, err := s.complete(ctx, ag, history)
	if err != nil {
		return Turn{}, err
	}

	s.mu.Lock()
	conv.messages = append(history, message{"assistant", reply})
	if keep := s.cfg.History * 2; len(conv.messages) > keep {
		conv.messages = conv.messages[len(conv.messages)-keep:]
	}
	s.mu.Unlock()

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
