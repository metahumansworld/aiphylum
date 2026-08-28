package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/judge"
	"github.com/singhtushant3-hub/aiphylum/internal/mind"
)

// AnthropicProvider forwards Messages-API calls to Anthropic. It is the
// custody boundary: the key lives here and nowhere an agent can reach.
type AnthropicProvider struct {
	APIKey  string
	BaseURL string // defaults to https://api.anthropic.com
	Client  *http.Client
}

func (a *AnthropicProvider) Invoke(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("provider call: %w", err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("read provider response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Usage{}, fmt.Errorf("provider returned %d: %.200s", resp.StatusCode, out)
	}

	// The provider's own usage block is the measurement of record.
	var parsed struct {
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, Usage{}, fmt.Errorf("provider response without usage: %w", err)
	}
	return out, parsed.Usage, nil
}

// StubProvider is a deterministic model for tests and the offline demo. Given
// the same request it produces the same response and the same usage, which is
// what lets `make demo` run seeded end-to-end with no key and no network.
//
// Its "reasoning" is a hash of the request: enough to give different prompts
// different answers, nothing more. The demo's reference agents treat its
// output as an oracle by construction (see generators/), so the economic loop
// is exercised for real even though no model is.
//
// It answers one question for real. A request that opens with the judge marker
// is asking it to grade a submission against a rubric, and it grades — by
// keyword containment, which is a dumb judge but a genuine one: the verdict is
// a function of what the agent actually wrote, so a careless agent fails
// offline for the same reason it would fail against a real grader. Nothing
// else in the stub inspects what it is asked.
type StubProvider struct {
	// Latency, if set, is added per call so demos have believable pacing.
	Latency time.Duration
}

func (s *StubProvider) Invoke(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	if s.Latency > 0 {
		select {
		case <-time.After(s.Latency):
		case <-ctx.Done():
			return nil, Usage{}, ctx.Err()
		}
	}

	var req struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, Usage{}, fmt.Errorf("stub: bad request: %w", err)
	}

	h := fnv.New64a()
	h.Write(body)
	digest := h.Sum64()

	// Usage figures derived from the request, deterministically: input scales
	// with the prompt, output is a bounded pseudo-random function of it.
	inputToks := int64(len(body) / 4)
	if inputToks < 1 {
		inputToks = 1
	}
	outputToks := int64(digest%64) + 16
	if outputToks > req.MaxTokens {
		outputToks = req.MaxTokens
	}

	var seed [8]byte
	binary.BigEndian.PutUint64(seed[:], digest)
	text := fmt.Sprintf("stub:%x", seed)
	if len(req.Messages) > 0 {
		var content string
		if err := json.Unmarshal(req.Messages[len(req.Messages)-1].Content, &content); err == nil {
			if graded, ok := stubGrade(content); ok {
				text = graded
			} else if said, ok := stubMind(content); ok {
				text = said
			}
		}
	}

	resp, err := json.Marshal(map[string]any{
		"id":    fmt.Sprintf("msg_stub_%016x", digest),
		"type":  "message",
		"role":  "assistant",
		"model": req.Model,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"stop_reason": "end_turn",
		"usage": Usage{
			InputTokens:  inputToks,
			OutputTokens: outputToks,
		},
	})
	if err != nil {
		return nil, Usage{}, err
	}
	return resp, Usage{InputTokens: inputToks, OutputTokens: outputToks}, nil
}

// stubGrade answers a grading request, or reports that this was not one. The
// tokens billed are unchanged — grading costs what any call of this size
// costs — so the judge wallet is metered exactly like an agent's.
func stubGrade(content string) (string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(content), judge.Marker) {
		return "", false
	}
	v := judge.Grade(judge.Section(content, judge.SecRubric), judge.Section(content, judge.SecSubmission))
	word := "fail"
	if v.Pass {
		word = "pass"
	}
	return fmt.Sprintf("VERDICT: %s\nREASON: %s", word, v.Reason), true
}

// stubMind answers a resident thinking or speaking, or reports that this was
// not one of those. Like stubGrade it changes only what comes back, never what
// the call costs — a resident's thought is metered exactly like an agent's
// attempt, which is the property that lets the town be pointed at a real model
// later without the accounting changing shape.
func stubMind(content string) (string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(content), mind.Marker) {
		return "", false
	}
	return mind.Answer(content), true
}
