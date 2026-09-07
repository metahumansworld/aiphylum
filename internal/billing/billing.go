// Package billing is the recharge: a person puts money in through Stripe
// Checkout, and Stripe tells the platform when it has been paid.
//
// Stripe is spoken to over two stdlib calls rather than a client library. A
// checkout is one form POST that answers with a URL to send the person to;
// the payment comes back as a webhook whose body is signed with a secret
// the platform holds. Nothing here touches a card: the person types it on
// Stripe's page, and the platform learns only that a session was paid and
// for how much. The amount minted is what Stripe says was paid, never what
// the checkout asked for.
package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/metahumansworld/soscitea/internal/account"
	"github.com/metahumansworld/soscitea/internal/ledger"
)

// Config wires the recharge to Stripe and to the users it credits.
type Config struct {
	// Key is the platform's Stripe secret key and WebhookSecret the signing
	// secret of its webhook endpoint. Both come from the environment and
	// never from a flag. Either one empty keeps billing closed: a key
	// without a webhook secret would take money and never credit it.
	Key, WebhookSecret string
	// Site is the public base URL the checkout returns to, with scheme and
	// host and nothing after: Stripe needs an absolute address.
	Site     string
	Accounts *account.Store
	Log      *slog.Logger
	// Stripe is the API base, replaceable for tests; empty means the real
	// one. HTTP is the client that reaches it; nil means a fifteen-second
	// timeout. Now is the clock the webhook's timestamp is checked against.
	Stripe string
	HTTP   *http.Client
	Now    func() time.Time
}

const (
	// A checkout is between $5 and $100. Stripe's fee on a US card is 2.9%
	// plus 30 cents, and it refuses anything under 50 cents; at a dollar the
	// fixed part alone is 30%, so the floor is where a topup stops being
	// mostly fee. The ceiling is a stranger's stolen card: the most one
	// checkout can lose.
	MinCents = 500
	MaxCents = 10_000

	stripeAPI = "https://api.stripe.com"
	// tolerance is how far a webhook's timestamp may be from the clock
	// before its signature is stale, Stripe's own recommendation.
	tolerance = 5 * time.Minute
)

// credits is what a cent paid buys, in credits (micro-USD). The fees are
// passed through (decided 2026-09-07): Stripe keeps 2.9% plus 30 cents of
// the charge, and OpenRouter keeps 5.5% when the platform buys the credits
// that cover the spend, so what lands is what survives both — a $5 topup
// is about $4.30 of model time. The person sees a balance in credits with
// no published dollar rate, so the fee surfaces only as the exchange rate.
// This is the pricing decision and the only place it is made. Integer
// arithmetic rounds down, so the platform never mints a fee away.
// ponytail: OpenRouter's 80-cent minimum is per platform purchase, not per
// topup — bulk buying amortises it; a floor here would double-charge it.
func credits(cents int64) ledger.Credits {
	afterStripe := cents*10_000*971/1000 - 30*10_000
	return ledger.Credits(afterStripe * 945 / 1000)
}

// Billing is the recharge surface.
type Billing struct {
	cfg Config
}

func New(cfg Config) *Billing {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Stripe == "" {
		cfg.Stripe = stripeAPI
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Site = strings.TrimRight(cfg.Site, "/")
	return &Billing{cfg: cfg}
}

// Open reports whether a checkout can be started.
func (b *Billing) Open() bool {
	return b.cfg.Key != "" && b.cfg.WebhookSecret != "" && b.cfg.Accounts != nil
}

// Handler is the recharge over HTTP.
//
//	GET  /billing                    → 200 {open, min, max}     cents; open false hides the button
//	POST /billing/checkout {cents}   → 200 {url}                send the person there; 503 when closed
//	POST /billing/webhook            → Stripe's endpoint, signed; never a person's
func (b *Billing) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /billing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"open": b.Open(), "min": MinCents, "max": MaxCents})
	})
	mux.HandleFunc("POST /billing/checkout", b.checkout)
	mux.HandleFunc("POST /billing/webhook", b.webhook)
	return mux
}

func (b *Billing) checkout(w http.ResponseWriter, r *http.Request) {
	if !b.Open() {
		httpError(w, http.StatusServiceUnavailable, "recharge is not open")
		return
	}
	u, err := b.cfg.Accounts.FromRequest(r)
	if errors.Is(err, account.ErrNoSession) {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read the session")
		return
	}
	var in struct {
		Cents int64 `json:"cents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if in.Cents < MinCents || in.Cents > MaxCents {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("a topup is between %d and %d cents", MinCents, MaxCents))
		return
	}
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("client_reference_id", u.ID)
	form.Set("success_url", b.cfg.Site+"/?topup=ok")
	form.Set("cancel_url", b.cfg.Site+"/")
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "usd")
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(in.Cents, 10))
	form.Set("line_items[0][price_data][product_data][name]", "soscitea credits")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, b.cfg.Stripe+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not open a checkout")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(b.cfg.Key, "")
	res, err := b.cfg.HTTP.Do(req)
	if err != nil {
		b.cfg.Log.Error("stripe checkout", "err", err)
		httpError(w, http.StatusBadGateway, "could not open a checkout")
		return
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var sess struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &sess) != nil || sess.URL == "" {
		b.cfg.Log.Error("stripe checkout", "status", res.Status, "body", string(body))
		httpError(w, http.StatusBadGateway, "could not open a checkout")
		return
	}
	b.cfg.Log.Info("checkout opened", "user", u.ID, "cents", in.Cents, "session", sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{"url": sess.URL})
}

// webhook is what Stripe calls. A payment is credited on a completed session
// that is paid, or on the later success of one that was not paid at
// completion (a bank transfer). Every other event is answered 200 and
// dropped: a code that is not 200 makes Stripe retry it for three days.
// Only a failure that a retry could mend — the books were unreachable —
// answers with one.
func (b *Billing) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		httpError(w, http.StatusBadRequest, "could not read the event")
		return
	}
	if !b.Open() || !verify(b.cfg.WebhookSecret, r.Header.Get("Stripe-Signature"), body, b.cfg.Now()) {
		httpError(w, http.StatusBadRequest, "bad signature")
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID       string `json:"id"`
				Status   string `json:"payment_status"`
				Currency string `json:"currency"`
				Amount   int64  `json:"amount_total"`
				User     string `json:"client_reference_id"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		httpError(w, http.StatusBadRequest, "event must be JSON")
		return
	}
	o := ev.Data.Object
	log := b.cfg.Log.With("event", ev.Type, "session", o.ID)
	switch {
	case ev.Type != "checkout.session.completed" && ev.Type != "checkout.session.async_payment_succeeded":
	case o.Status != "paid":
		log.Info("session not paid yet", "status", o.Status)
	// The credits guard, not an amount guard: below ~31¢ the fixed fee eats
	// the whole charge and the mint would be zero or negative. Unreachable
	// through the platform's own checkout (MinCents is 500), but a session
	// made with the same key by hand is not the platform's checkout.
	case o.Currency != "usd" || o.User == "" || credits(o.Amount) <= 0:
		log.Error("paid session the platform cannot credit; refund it by hand", "currency", o.Currency, "amount", o.Amount, "user", o.User)
	default:
		err := b.cfg.Accounts.Recharge(r.Context(), o.User, o.ID, credits(o.Amount))
		if errors.Is(err, account.ErrNoSuchUser) {
			log.Error("paid session for a user that does not exist; refund it by hand", "user", o.User)
		} else if err != nil {
			log.Error("recharge failed; stripe will retry", "err", err)
			httpError(w, http.StatusInternalServerError, "could not credit the payment")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// verify checks a Stripe-Signature header — "t=<unix>,v1=<hex>", with more
// than one v1 while a secret is being rotated — against the body. The
// signed string is the timestamp, a dot, and the body as it arrived.
func verify(secret, header string, body []byte, now time.Time) bool {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return false
	}
	if d := now.Unix() - t; d > int64(tolerance.Seconds()) || d < -int64(tolerance.Seconds()) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
