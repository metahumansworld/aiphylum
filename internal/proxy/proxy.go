// Package proxy is the metering LLM proxy: the only route out of an agent
// container, and therefore the one place where token counts are measured
// rather than self-reported.
//
// Every model call follows the same arc: authenticate the caller to a wallet,
// price the worst case, reserve it (refusing if the wallet cannot cover it),
// invoke the provider with a key the agent never sees, settle at the measured
// usage, and record the whole exchange for the trace. Failure attribution
// falls out of that arc — a provider or proxy fault releases the hold
// (platform fault, agent charged nothing), while everything the agent caused
// settles at cost.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/metahumansworld/aiphylum/internal/ledger"
)

var (
	ErrModelNotAllowed = errors.New("proxy: model not on allowlist")
	ErrUnknownToken    = errors.New("proxy: unknown agent token")
	ErrCallTooLarge    = errors.New("proxy: call exceeds per-call ceiling")
)

// Usage is what the provider reports having measured for one call.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Provider turns a request body into a response body, using a key the caller
// never holds. Implementations exist for real providers and for the seeded
// stub the demo runs on.
type Provider interface {
	// Invoke sends the raw Messages-API request body to the model named in it
	// and returns the raw response body plus the provider's own usage figures.
	Invoke(ctx context.Context, model string, body []byte) (resp []byte, usage Usage, err error)
}

// Outcome classifies how a call ended, and with it who pays.
type Outcome string

const (
	OutcomeOK            Outcome = "ok"             // settled at measured cost
	OutcomeRefused       Outcome = "refused"        // never reached the provider; nothing moved
	OutcomePlatformFault Outcome = "platform_fault" // hold released; agent charged nothing
)

// Event is the proxy's record of one call — the raw material of a trace and
// of replay. Request and response are stored verbatim: replay is exact because
// this is the byte stream that actually happened.
type Event struct {
	Time     time.Time       `json:"time"`
	Wallet   string          `json:"wallet"`
	Model    string          `json:"model"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
	Usage    Usage           `json:"usage"`
	Cost     ledger.Credits  `json:"cost"`
	Balance  ledger.Credits  `json:"balance"` // wallet balance after the call
	Outcome  Outcome         `json:"outcome"`
	Error    string          `json:"error,omitempty"`
}

// Recorder receives every proxy event. The trace package implements it with a
// JSONL writer; tests implement it with a slice.
type Recorder interface {
	Record(ev Event)
}

// discardRecorder drops events, for callers that have not wired a trace yet.
type discardRecorder struct{}

func (discardRecorder) Record(Event) {}

// Proxy meters model calls against wallets. It is an http.Handler; the runner
// exposes it to containers as their only reachable address.
type Proxy struct {
	Ledger   *ledger.Ledger
	Table    *PriceTable
	Provider Provider
	Recorder Recorder
	Log      *slog.Logger

	// PerCallCeiling caps the worst-case cost of any single call. Zero means
	// no per-call cap beyond the wallet itself.
	PerCallCeiling ledger.Credits

	mu     sync.RWMutex
	tokens map[string]string // bearer token -> wallet ID
}

// New assembles a proxy. Provider and Ledger are required; a nil Recorder
// discards events.
func New(l *ledger.Ledger, t *PriceTable, p Provider, r Recorder, log *slog.Logger) *Proxy {
	if r == nil {
		r = discardRecorder{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{
		Ledger:   l,
		Table:    t,
		Provider: p,
		Recorder: r,
		Log:      log,
		tokens:   map[string]string{},
	}
}

// Authorize binds a bearer token to a wallet. The runner mints a fresh token
// per container and injects it as the container's only credential.
func (p *Proxy) Authorize(token, walletID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens[token] = walletID
}

// Revoke invalidates a token, e.g. when its container is torn down.
func (p *Proxy) Revoke(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tokens, token)
}

func (p *Proxy) walletFor(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		return "", ErrUnknownToken
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	wallet, ok := p.tokens[token]
	if !ok {
		return "", ErrUnknownToken
	}
	return wallet, nil
}

// messagesRequest is the slice of the Messages API the proxy needs to price a
// call; everything else passes through untouched.
type messagesRequest struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`
}

// estimateInputTokens bounds the input token count from above. One token per
// byte over-reserves several-fold for ordinary text, but the surplus returns
// at settlement, and an upper bound is what keeps settlement from ever
// exceeding the hold.
func estimateInputTokens(body []byte) int64 {
	return int64(len(body)) + 16
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.handleMessages(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		p.handleModels(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/wallet":
		p.handleWallet(w, r)
	default:
		httpError(w, http.StatusNotFound, "no such endpoint")
	}
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := p.walletFor(r); err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": p.Table.Models()})
}

func (p *Proxy) handleWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := p.walletFor(r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}
	bal, err := p.Ledger.Balance(r.Context(), wallet)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "wallet lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallet": wallet, "balance": bal})
}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	wallet, err := p.walletFor(r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "unreadable body")
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" || req.MaxTokens <= 0 {
		p.refuse(w, wallet, req.Model, body, http.StatusBadRequest,
			"body must be JSON with model and positive max_tokens")
		return
	}

	price, ok := p.Table.Lookup(req.Model)
	if !ok {
		p.refuse(w, wallet, req.Model, body, http.StatusForbidden,
			fmt.Sprintf("model %q is not on the allowlist", req.Model))
		return
	}

	// Price the worst case and reserve it. This is where an empty wallet is
	// refused: the hold fails before any provider is contacted.
	worst := price.Cost(estimateInputTokens(body), req.MaxTokens)
	if p.PerCallCeiling > 0 && worst > p.PerCallCeiling {
		p.refuse(w, wallet, req.Model, body, http.StatusForbidden,
			fmt.Sprintf("worst-case cost %d exceeds per-call ceiling %d", worst, p.PerCallCeiling))
		return
	}
	hold, err := p.Ledger.Hold(ctx, wallet, worst, "call:"+req.Model)
	if err != nil {
		status := http.StatusInternalServerError
		msg := "reservation failed"
		switch {
		case errors.Is(err, ledger.ErrInsufficient):
			status, msg = http.StatusPaymentRequired, "insufficient credits for worst-case cost"
		case errors.Is(err, ledger.ErrClosed):
			status, msg = http.StatusGone, "wallet is retired"
		case errors.Is(err, ledger.ErrNotFound):
			status, msg = http.StatusGone, "wallet does not exist"
		}
		p.refuse(w, wallet, req.Model, body, status, msg)
		return
	}

	resp, usage, err := p.Provider.Invoke(ctx, req.Model, body)
	if err != nil {
		// The provider failing is not the agent's doing: release everything.
		if rerr := p.Ledger.Release(ctx, hold.ID); rerr != nil {
			p.Log.Error("release after provider fault failed", "hold", hold.ID, "err", rerr)
		}
		bal, _ := p.Ledger.Balance(ctx, wallet)
		p.Recorder.Record(Event{
			Time: time.Now(), Wallet: wallet, Model: req.Model,
			Request: body, Outcome: OutcomePlatformFault, Error: err.Error(), Balance: bal,
		})
		httpError(w, http.StatusBadGateway, "provider unavailable; nothing was charged")
		return
	}

	cost := price.Cost(usage.InputTokens, usage.OutputTokens)
	if cost > hold.Amount {
		// The estimate was meant to be an upper bound; if a provider ever
		// reports past it, charge the reserved worst case and log loudly
		// rather than either overdraw the wallet or corrupt the settlement.
		p.Log.Error("usage exceeded reservation; charging the reservation",
			"wallet", wallet, "model", req.Model, "cost", cost, "held", hold.Amount)
		cost = hold.Amount
	}
	if err := p.Ledger.Settle(ctx, hold.ID, cost); err != nil {
		p.Log.Error("settle failed", "hold", hold.ID, "err", err)
		httpError(w, http.StatusInternalServerError, "settlement failed")
		return
	}

	bal, _ := p.Ledger.Balance(ctx, wallet)
	p.Recorder.Record(Event{
		Time: time.Now(), Wallet: wallet, Model: req.Model,
		Request: body, Response: resp, Usage: usage, Cost: cost, Balance: bal,
		Outcome: OutcomeOK,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Phylum-Cost", fmt.Sprint(int64(cost)))
	w.Header().Set("X-Phylum-Balance", fmt.Sprint(int64(bal)))
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

// refuse records and reports a call that never reached the provider. Nothing
// has moved in the ledger on this path.
func (p *Proxy) refuse(w http.ResponseWriter, wallet, model string, body []byte, status int, msg string) {
	bal, _ := p.Ledger.Balance(context.Background(), wallet)
	p.Recorder.Record(Event{
		Time: time.Now(), Wallet: wallet, Model: model,
		Request: body, Outcome: OutcomeRefused, Error: msg, Balance: bal,
	})
	httpError(w, status, msg)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
