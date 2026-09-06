package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenRouter is reached by the Anthropic provider with only the base URL
// changed: same path under the base, same key header, same usage block read
// back. This pins the shape of the request the provider emits so that a live
// run against OpenRouter differs from this test in the host name alone.
func TestAnthropicProviderReachesOpenRouterUnchanged(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotVersion = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"gen-1","type":"message","role":"assistant","model":"anthropic/claude-haiku-4.5",` +
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":12,"output_tokens":3,"cost":0.000027}}`))
	}))
	defer srv.Close()

	// The same suffix OpenRouterBaseURL carries, so the path under test is the
	// path a live call takes.
	base := srv.URL + strings.TrimPrefix(OpenRouterBaseURL, "https://openrouter.ai")
	p := &AnthropicProvider{APIKey: "sk-or-test", BaseURL: base}
	resp, usage, err := p.Invoke(context.Background(), "anthropic/claude-haiku-4.5",
		[]byte(`{"model":"anthropic/claude-haiku-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if gotPath != "/api/v1/messages" {
		t.Errorf("path = %q, want /api/v1/messages", gotPath)
	}
	if gotKey != "sk-or-test" || gotVersion == "" {
		t.Errorf("headers: x-api-key=%q anthropic-version=%q", gotKey, gotVersion)
	}
	if usage != (Usage{InputTokens: 12, OutputTokens: 3}) {
		t.Errorf("usage = %+v; OpenRouter's extra fields should not disturb the measurement", usage)
	}
	if !strings.Contains(string(resp), `"text":"hello"`) {
		t.Errorf("response not passed through verbatim: %s", resp)
	}
}
