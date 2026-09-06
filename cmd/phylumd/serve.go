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

	"github.com/metahumansworld/aiphylum/internal/account"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/service"
	"github.com/metahumansworld/aiphylum/internal/spec"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// openRouterKeyEnv names the platform's OpenRouter key. It is read from the
// environment and never from a flag, so it cannot end up in a shell history
// or a process listing.
const openRouterKeyEnv = "OPENROUTER_API_KEY"

// operator owns the agents loaded from -agent files at boot. It is a user
// like any other to the service — one wallet, funded once with the grant —
// so the boot agents are capped the way a person's are, and the ledger's
// audit sees one kind of wallet.
var operator = service.Owner{ID: "operator", Wallet: "usr:operator"}

// runServe runs built agents: the proxy in front of one provider, the service
// in front of the proxy, the account store in front of the service's control
// surface, and one HTTP listener carrying all of it. The price table decides
// what the agents may call. With no key it holds the stub's model beside the
// real one, so a spec naming the real model runs offline unchanged — the stub
// answers under whatever name it is asked for, and the accounting is the same
// either way.
func runServe(ctx context.Context, log *slog.Logger, l *ledger.Ledger, tw *trace.Writer, opt options) error {
	model, price, err := parseModelPrice(opt.serveModel)
	if err != nil {
		return err
	}
	table := proxy.NewPriceTable()
	table.Set(model, price)

	var prov proxy.Provider
	if key := os.Getenv(openRouterKeyEnv); key != "" {
		prov = &proxy.AnthropicProvider{APIKey: key, BaseURL: proxy.OpenRouterBaseURL}
		log.Info("service is live: real spend behind each user's grant",
			"provider", "openrouter", "model", model, "grant", ledger.Credits(opt.grant))
	} else {
		table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
		prov = &proxy.StubProvider{Latency: opt.latency}
		log.Info("service is offline: the stub answers every model, zero API calls",
			"hint", "set "+openRouterKeyEnv+" for real replies")
	}
	p := proxy.New(l, table, prov, tw, log)
	locked := splitList(opt.serveLocked)
	svc := service.New(service.Config{Proxy: p, Ledger: l, Log: log, Locked: locked})

	// The mailer is the one launch dependency not chosen yet; until it is,
	// the link goes to the log, and the operator at the terminal is the mail.
	reasons := []string{"arena"}
	for _, m := range locked {
		reasons = append(reasons, "model:"+m)
	}
	accounts, err := account.Open(opt.accountsPath, account.Config{
		Ledger: l, Grant: ledger.Credits(opt.grant), Mailer: account.LogMailer{Log: log, Addr: opt.serveListen},
		Log: log, Reasons: reasons,
	})
	if err != nil {
		return err
	}
	defer accounts.Close()

	if len(opt.agents) > 0 {
		if err := fundOperator(ctx, l, ledger.Credits(opt.grant)); err != nil {
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
	mux.Handle("/v1/", svc.Control(auth))
	mux.Handle("/a/", svc.Public())
	srv := &http.Server{Addr: opt.serveListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("service listening", "addr", opt.serveListen, "sign-in", "/auth/request", "control", "/v1/agents", "public", "/a/{id}")
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
