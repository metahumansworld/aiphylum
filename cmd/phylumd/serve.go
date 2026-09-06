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

// runServe runs built agents: the proxy in front of one provider, the service
// in front of the proxy, and one HTTP listener carrying both of the service's
// surfaces. The price table decides what the agents may call. With no key it
// holds the stub's model beside the real one, so a spec naming the real model
// runs offline unchanged — the stub answers under whatever name it is asked
// for, and the accounting is the same either way.
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
		log.Info("service is live: real spend behind each agent's grant",
			"provider", "openrouter", "model", model, "grant", ledger.Credits(opt.grant))
	} else {
		table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
		prov = &proxy.StubProvider{Latency: opt.latency}
		log.Info("service is offline: the stub answers every model, zero API calls",
			"hint", "set "+openRouterKeyEnv+" for real replies")
	}
	p := proxy.New(l, table, prov, tw, log)
	svc := service.New(service.Config{Proxy: p, Ledger: l, Grant: ledger.Credits(opt.grant), Log: log})

	for _, path := range opt.agents {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read agent spec: %w", err)
		}
		a, err := spec.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		ag, err := svc.Create(ctx, a)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		log.Info("agent is reachable", "agent", ag.ID, "name", a.Name,
			"url", "http://"+opt.serveListen+"/a/"+ag.ID)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", svc.Control())
	mux.Handle("/a/", svc.Public())
	srv := &http.Server{Addr: opt.serveListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("service listening", "addr", opt.serveListen, "control", "/v1/agents", "public", "/a/{id}")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
