package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The fixture is OpenRouter's real listing, cut to the launch models and one
// decoy priced "-1": what the catalogue says for a router whose price varies.
func TestSyncPricesReadsTheCatalogueAndSkipsTheUnpriced(t *testing.T) {
	fixture, err := os.ReadFile("testdata/openrouter-models.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	table := NewPriceTable()
	table.Set("anthropic/claude-haiku-4.5", Price{InputPerTok: 1, OutputPerTok: 1}) // a hand price the sync must replace
	table.Set("stub-1", Price{InputPerTok: 1000, OutputPerTok: 1000})
	missing, err := SyncPrices(context.Background(), srv.Client(), srv.URL, table,
		[]string{"anthropic/claude-haiku-4.5", "anthropic/claude-opus-5", "openrouter/auto", "nobody/nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(missing), 2; got != want || missing[0] != "openrouter/auto" || missing[1] != "nobody/nothing" {
		t.Fatalf("missing = %v, want the unpriced router and the unknown model", missing)
	}
	haiku, _ := table.Lookup("anthropic/claude-haiku-4.5")
	if haiku != (Price{InputPerTok: 1000, OutputPerTok: 5000}) {
		t.Fatalf("haiku = %+v, want $1 and $5 per million in nano-USD", haiku)
	}
	opus, _ := table.Lookup("anthropic/claude-opus-5")
	if opus != (Price{InputPerTok: 5000, OutputPerTok: 25000}) {
		t.Fatalf("opus = %+v", opus)
	}
	if _, ok := table.Lookup("openrouter/auto"); ok {
		t.Fatal("an unpriced model must not reach the table")
	}
	if _, ok := table.Lookup("stub-1"); !ok {
		t.Fatal("the sync must leave models it was not asked about alone")
	}
}

func TestSyncPricesFailsClosedOnABadCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	table := NewPriceTable()
	table.Set("m", Price{InputPerTok: 7, OutputPerTok: 7})
	if _, err := SyncPrices(context.Background(), srv.Client(), srv.URL, table, []string{"m"}); err == nil {
		t.Fatal("want an error from a 502 listing")
	}
	if p, _ := table.Lookup("m"); p.InputPerTok != 7 {
		t.Fatal("a failed sync must not touch the table")
	}
}
