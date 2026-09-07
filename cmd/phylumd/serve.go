package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/metahumansworld/soscitea/internal/account"
	"github.com/metahumansworld/soscitea/internal/billing"
	"github.com/metahumansworld/soscitea/internal/builder"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/service"
	"github.com/metahumansworld/soscitea/internal/spec"
	"github.com/metahumansworld/soscitea/internal/trace"
	"github.com/metahumansworld/soscitea/internal/widget"
)

// openRouterKeyEnv names the platform's OpenRouter key. It is read from the
// environment and never from a flag, so it cannot end up in a shell history
// or a process listing.
const openRouterKeyEnv = "OPENROUTER_API_KEY"

// The Stripe secrets, read the same way. Both must be set for the recharge
// to open: a key alone would take money the platform never hears about.
const (
	stripeKeyEnv     = "STRIPE_SECRET_KEY"
	stripeWebhookEnv = "STRIPE_WEBHOOK_SECRET"
)

// The mail relay, read the same way. Addr and from open real mail together;
// user and pass are optional, for a relay that asks no login.
const (
	smtpAddrEnv = "SMTP_ADDR"
	smtpUserEnv = "SMTP_USER"
	smtpPassEnv = "SMTP_PASS"
	mailFromEnv = "MAIL_FROM"
)

// operator owns the agents loaded from -agent files at boot. It is a user
// like any other to the service — one wallet, funded once with the grant —
// so the boot agents are capped the way a person's are, and the ledger's
// audit sees one kind of wallet.
var operator = service.Owner{ID: "operator", Wallet: "usr:operator"}

// operatorGrant funds the operator's wallet when -agent files are loaded at
// boot, so those agents can answer. It is the operator's own spend — real
// only when the operator sets a real key. Users get no such grant: a new
// wallet starts empty and fills at the recharge.
const operatorGrant = ledger.Credits(1_000_000) // $1

// runServe runs built agents: the proxy in front of one provider, the service
// in front of the proxy, the account store in front of the service's control
// surface, the builder page in front of a person, and one HTTP listener
// carrying all of it. The one real model is also the builder's: a draft is
// a call on it like a reply is, from the same wallet. The price table decides
// what the agents may call: the offered model and the locked ones, priced
// live from OpenRouter's catalogue, with the -serve-model price standing in
// for the one model when the catalogue cannot be read. With no key it holds
// the stub's model beside the real ones at the stub's price, so a spec
// naming a real model runs offline unchanged — the stub answers under
// whatever name it is asked for, and the accounting is the same either way.
func runServe(ctx context.Context, log *slog.Logger, l *ledger.Ledger, tw *trace.Writer, opt options) error {
	model, price, err := parseModelPrice(opt.serveModel)
	if err != nil {
		return err
	}
	table := proxy.NewPriceTable()
	table.Set(model, price)
	locked := splitList(opt.serveLocked)

	var prov proxy.Provider
	if key := os.Getenv(openRouterKeyEnv); key != "" {
		prov = &proxy.AnthropicProvider{APIKey: key, BaseURL: proxy.OpenRouterBaseURL}
		missing, err := proxy.SyncPrices(ctx, &http.Client{Timeout: 15 * time.Second}, proxy.OpenRouterBaseURL, table, append([]string{model}, locked...))
		if err != nil {
			log.Warn("could not read openrouter's catalogue; the locked models stay off the table", "err", err)
		} else if len(missing) > 0 {
			log.Warn("openrouter's catalogue does not price these models; a spec naming one is refused", "models", missing)
		}
		log.Info("service is live: real spend behind each user's credits",
			"provider", "openrouter", "model", model)
	} else {
		stub := proxy.Price{InputPerTok: 1000, OutputPerTok: 1000}
		for _, m := range append([]string{"stub-1"}, locked...) {
			table.Set(m, stub)
		}
		prov = &proxy.StubProvider{Latency: opt.latency}
		log.Info("service is offline: the stub answers every model, zero API calls",
			"hint", "set "+openRouterKeyEnv+" for real replies")
	}
	p := proxy.New(l, table, prov, tw, log)

	// The site is the public face: sign-in links and Stripe's return URLs
	// point at it, and the listen address stands in when none is named.
	site := opt.serveSite
	if site == "" {
		site = "http://" + opt.serveListen
	}

	// Real mail opens when the relay and the sender are both named; without
	// them the link goes to the log, and the operator at the terminal is the
	// mail.
	var mailer account.Mailer = account.LogMailer{Log: log, Addr: opt.serveListen}
	if addr, from := os.Getenv(smtpAddrEnv), os.Getenv(mailFromEnv); addr != "" && from != "" {
		mailer = &account.SMTPMailer{Addr: addr, From: from,
			User: os.Getenv(smtpUserEnv), Pass: os.Getenv(smtpPassEnv), Site: site, Log: log}
		log.Info("mail is live: sign-in links go through the relay", "relay", addr, "from", from)
	} else {
		log.Info("mail is offline: sign-in links go to the log", "hint", "set "+smtpAddrEnv+" and "+mailFromEnv)
		if os.Getenv(openRouterKeyEnv) != "" {
			log.Warn("the service is live but its mail is not: nobody signs in without reading the log")
		}
	}

	reasons := []string{"arena"}
	for _, m := range locked {
		reasons = append(reasons, "model:"+m)
	}
	// No Grant: a new user's wallet starts empty. Credits come from the
	// recharge, and until then the locked models' waitlist is the offer.
	accounts, err := account.Open(opt.accountsPath, account.Config{
		Ledger: l, Mailer: mailer,
		Log: log, Reasons: reasons,
	})
	if err != nil {
		return err
	}
	defer accounts.Close()

	store, err := service.OpenStore(opt.agentsPath)
	if err != nil {
		return err
	}
	defer store.Close()
	svc, err := service.New(service.Config{Proxy: p, Ledger: l, Log: log, Locked: locked, Unlocked: accounts.Paid,
		Store: store, BuilderModel: model, InsecureTools: opt.serveInsecureTools})
	if err != nil {
		return err
	}
	if opt.serveInsecureTools {
		log.Warn("tools may reach http and private addresses: -serve-insecure-tools is for your own machine only")
	}

	// The recharge is open when both Stripe secrets are in the environment,
	// and the builder hides its button when it is not.
	pay := billing.New(billing.Config{Key: os.Getenv(stripeKeyEnv), WebhookSecret: os.Getenv(stripeWebhookEnv),
		Site: site, Accounts: accounts, Log: log})
	if pay.Open() {
		log.Info("recharge is open", "site", site, "webhook", site+"/billing/webhook")
	} else {
		log.Info("recharge is closed: locked models join the waitlist", "hint", "set "+stripeKeyEnv+" and "+stripeWebhookEnv)
	}

	// The operator's agents come from files, and the files are the truth:
	// what a previous boot stored for the operator is dropped and made again
	// from the same files, so -agent is idempotent across restarts.
	if len(opt.agents) > 0 {
		if err := fundOperator(ctx, l, operatorGrant); err != nil {
			return err
		}
	}
	for _, ag := range svc.AgentsOf(operator.ID) {
		if err := svc.Delete(ctx, ag.ID); err != nil {
			return err
		}
	}
	for _, path := range opt.agents {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read agent spec: %w", err)
		}
		a, err := spec.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		ag, err := svc.Create(ctx, operator, a)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		log.Info("agent is reachable", "agent", ag.ID, "name", a.Name,
			"url", "http://"+opt.serveListen+"/a/"+ag.ID)
	}

	// A session is an owner. The account store's "not signed in" is the
	// service's 401; anything else it says is a failure and answers as one.
	auth := func(r *http.Request) (service.Owner, error) {
		u, err := accounts.FromRequest(r)
		if errors.Is(err, account.ErrNoSession) {
			return service.Owner{}, service.ErrUnauthenticated
		}
		if err != nil {
			return service.Owner{}, err
		}
		return service.Owner{ID: u.ID, Wallet: u.Wallet}, nil
	}

	mux := http.NewServeMux()
	mux.Handle("/auth/", accounts.Handler())
	mux.Handle("/waitlist", accounts.Handler())
	mux.Handle("/billing", pay.Handler())
	mux.Handle("/billing/", pay.Handler())
	mux.Handle("/v1/", svc.Control(auth))
	mux.Handle("/a/", svc.Public())
	mux.Handle("GET /a/{id}/embed", widget.Handler())
	mux.Handle("GET /widget.js", widget.Handler())
	mux.Handle("/", builder.Handler())
	srv := &http.Server{Addr: opt.serveListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("service listening", "addr", opt.serveListen, "builder", "http://"+opt.serveListen+"/", "control", "/v1/agents", "public", "/a/{id}", "widget", "/widget.js")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// fundOperator gives the operator its wallet, once. On a persistent ledger
// the second boot finds it and leaves it as it is: the grant is a grant, not
// a salary.
func fundOperator(ctx context.Context, l *ledger.Ledger, grant ledger.Credits) error {
	err := l.CreateAccount(ctx, operator.Wallet, ledger.KindUser, operator.ID)
	if errors.Is(err, ledger.ErrAccountExists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("operator wallet: %w", err)
	}
	if grant > 0 {
		if _, err := l.Mint(ctx, operator.Wallet, grant, "grant", operator.ID); err != nil {
			return fmt.Errorf("operator grant: %w", err)
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseModelPrice reads "id=input,output": a model id and its two prices in
// nano-USD per token, the units the price table keeps.
func parseModelPrice(s string) (string, proxy.Price, error) {
	id, prices, ok := strings.Cut(s, "=")
	in, out, ok2 := strings.Cut(prices, ",")
	if !ok || !ok2 || strings.TrimSpace(id) == "" {
		return "", proxy.Price{}, fmt.Errorf("-serve-model %q: want id=input,output in nano-USD per token", s)
	}
	inTok, err1 := strconv.ParseInt(strings.TrimSpace(in), 10, 64)
	outTok, err2 := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err1 != nil || err2 != nil || inTok < 0 || outTok < 0 {
		return "", proxy.Price{}, fmt.Errorf("-serve-model %q: prices must be non-negative integers", s)
	}
	return strings.TrimSpace(id), proxy.Price{InputPerTok: inTok, OutputPerTok: outTok}, nil
}
