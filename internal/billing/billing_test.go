package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/aiphylum/internal/account"
	"github.com/metahumansworld/aiphylum/internal/ledger"
)

type memMailer struct{ token string }

func (m *memMailer) Send(_ context.Context, _, token string) error { m.token = token; return nil }

type harness struct {
	b       *Billing
	h       http.Handler
	l       *ledger.Ledger
	user    account.User
	bearer  string
	forms   []url.Values
	now     time.Time
	secret  string
	stripe  *httptest.Server
	nextErr int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	mail := &memMailer{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts, err := account.Open(filepath.Join(dir, "accounts.db"), account.Config{Ledger: l, Grant: 1_000_000, Mailer: mail, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accounts.Close() })
	if err := accounts.Request(context.Background(), "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	sess, err := accounts.Verify(context.Background(), mail.token)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{l: l, user: sess.User, bearer: sess.Token, secret: "whsec_test", now: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	h.stripe = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != "sk_test" || r.URL.Path != "/v1/checkout/sessions" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.forms = append(h.forms, r.PostForm)
		if h.nextErr != 0 {
			w.WriteHeader(h.nextErr)
			return
		}
		fmt.Fprintf(w, `{"id":"cs_test_%d","url":"https://checkout.stripe.com/c/pay/cs_test_%d"}`, len(h.forms), len(h.forms))
	}))
	t.Cleanup(h.stripe.Close)
	h.b = New(Config{Key: "sk_test", WebhookSecret: h.secret, Site: "https://soscitea.test/", Accounts: accounts,
		Log: quiet, Stripe: h.stripe.URL, HTTP: h.stripe.Client(), Now: func() time.Time { return h.now }})
	h.h = h.b.Handler()
	return h
}

func (h *harness) do(method, path, bearer string, body any, hdr ...string) (int, map[string]any) {
	var buf bytes.Buffer
	if s, ok := body.(string); ok {
		buf.WriteString(s)
	} else if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func sign(secret string, body string, at time.Time) string {
	ts := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *harness) event(typ, status, currency string, cents int64, session string) string {
	return fmt.Sprintf(`{"type":%q,"data":{"object":{"id":%q,"payment_status":%q,"currency":%q,"amount_total":%d,"client_reference_id":%q}}}`,
		typ, session, status, currency, cents, h.user.ID)
}

func (h *harness) balance(t *testing.T) ledger.Credits {
	t.Helper()
	bal, err := h.l.Balance(context.Background(), h.user.Wallet)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

func TestCheckoutOpensAStripeSessionForTheSignedInUser(t *testing.T) {
	h := newHarness(t)
	code, out := h.do("POST", "/billing/checkout", h.bearer, map[string]any{"cents": 1000})
	if code != 200 || out["url"] != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Fatalf("checkout: %d %v", code, out)
	}
	f := h.forms[0]
	for k, want := range map[string]string{
		"mode": "payment", "client_reference_id": h.user.ID,
		"success_url": "https://soscitea.test/?topup=ok", "cancel_url": "https://soscitea.test/",
		"line_items[0][price_data][currency]": "usd", "line_items[0][price_data][unit_amount]": "1000",
		"line_items[0][quantity]": "1",
	} {
		if got := f.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
	for _, cents := range []int64{MinCents - 1, MaxCents + 1, 0, -500} {
		if code, _ := h.do("POST", "/billing/checkout", h.bearer, map[string]any{"cents": cents}); code != 400 {
			t.Errorf("%d cents: %d, want 400", cents, code)
		}
	}
	if code, _ := h.do("POST", "/billing/checkout", "", map[string]any{"cents": 1000}); code != 401 {
		t.Errorf("no session: %d, want 401", code)
	}
	if code, _ := h.do("POST", "/billing/checkout", h.bearer, "not json"); code != 400 {
		t.Errorf("bad body: %d, want 400", code)
	}
	h.nextErr = 500
	if code, _ := h.do("POST", "/billing/checkout", h.bearer, map[string]any{"cents": 1000}); code != 502 {
		t.Errorf("stripe down: %d, want 502", code)
	}
	if len(h.forms) != 2 {
		t.Errorf("stripe was called %d times, want 2: the refused amounts never reach it", len(h.forms))
	}
}

func TestWebhookCreditsAPaidSessionOnce(t *testing.T) {
	h := newHarness(t)
	paid := h.event("checkout.session.completed", "paid", "usd", 1000, "cs_a")
	for range 2 {
		if code, out := h.do("POST", "/billing/webhook", "", paid, "Stripe-Signature", sign(h.secret, paid, h.now)); code != 200 {
			t.Fatalf("webhook: %d %v", code, out)
		}
	}
	if got := h.balance(t); got != 1_000_000+credits(1000) {
		t.Fatalf("balance = %d, want the grant plus one $10 topup", got)
	}
	later := h.event("checkout.session.async_payment_succeeded", "paid", "usd", 500, "cs_b")
	if code, _ := h.do("POST", "/billing/webhook", "", later, "Stripe-Signature", sign(h.secret, later, h.now)); code != 200 {
		t.Fatal("a bank payment that succeeds later is a payment")
	}
	if got := h.balance(t); got != 1_000_000+credits(1500) {
		t.Fatalf("balance = %d after the bank payment", got)
	}
}

func TestWebhookRefusesWhatIsNotAPaidSignedUSDSession(t *testing.T) {
	h := newHarness(t)
	paid := h.event("checkout.session.completed", "paid", "usd", 1000, "cs_a")
	cases := []struct {
		name string
		body string
		sig  string
		code int
	}{
		{"tampered body", strings.Replace(paid, "1000", "9000", 1), sign(h.secret, paid, h.now), 400},
		{"wrong secret", paid, sign("whsec_other", paid, h.now), 400},
		{"stale", paid, sign(h.secret, paid, h.now.Add(-tolerance-time.Second)), 400},
		{"no header", paid, "", 400},
		{"not paid yet", h.event("checkout.session.completed", "unpaid", "usd", 1000, "cs_b"), "", 200},
		{"euros", h.event("checkout.session.completed", "paid", "eur", 1000, "cs_c"), "", 200},
		{"free", h.event("checkout.session.completed", "paid", "usd", 0, "cs_d"), "", 200},
		{"other event", h.event("payment_intent.created", "paid", "usd", 1000, "cs_e"), "", 200},
		{"stranger", strings.Replace(paid, h.user.ID, "u_nobody", 1), "", 200},
	}
	for _, c := range cases {
		sig := c.sig
		if sig == "" && c.code == 200 {
			sig = sign(h.secret, c.body, h.now)
		}
		if code, out := h.do("POST", "/billing/webhook", "", c.body, "Stripe-Signature", sig); code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.code)
		}
	}
	if got := h.balance(t); got != 1_000_000 {
		t.Fatalf("balance = %d: none of those may mint", got)
	}
	// Rotation: an older v1 beside the current one still verifies.
	two := "t=" + fmt.Sprint(h.now.Unix()) + ",v1=deadbeef," + strings.TrimPrefix(sign(h.secret, paid, h.now), "t="+fmt.Sprint(h.now.Unix())+",")
	if code, _ := h.do("POST", "/billing/webhook", "", paid, "Stripe-Signature", two); code != 200 {
		t.Errorf("two signatures, one right: %d", code)
	}
}

func TestBillingIsClosedWithoutBothSecrets(t *testing.T) {
	h := newHarness(t)
	closed := New(Config{Key: "sk_test", Accounts: h.b.cfg.Accounts, Log: h.b.cfg.Log}).Handler()
	r := httptest.NewRequest("GET", "/billing", nil)
	w := httptest.NewRecorder()
	closed.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["open"] != false || out["min"] != float64(MinCents) {
		t.Fatalf("closed billing says %v", out)
	}
	r = httptest.NewRequest("POST", "/billing/checkout", strings.NewReader(`{"cents":1000}`))
	r.Header.Set("Authorization", "Bearer "+h.bearer)
	w = httptest.NewRecorder()
	closed.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("checkout while closed: %d, want 503", w.Code)
	}
	paid := h.event("checkout.session.completed", "paid", "usd", 1000, "cs_a")
	r = httptest.NewRequest("POST", "/billing/webhook", strings.NewReader(paid))
	r.Header.Set("Stripe-Signature", sign("", paid, time.Now()))
	w = httptest.NewRecorder()
	closed.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("webhook while closed: %d, want 400: no secret verifies nothing", w.Code)
	}
	if code, out := h.do("GET", "/billing", "", nil); code != 200 || out["open"] != true {
		t.Fatalf("open billing says %d %v", code, out)
	}
}
