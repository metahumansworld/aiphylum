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

	"github.com/metahumansworld/soscitea/internal/judge"
	"github.com/metahumansworld/soscitea/internal/mind"
	"github.com/metahumansworld/soscitea/internal/spec"
)

// OpenRouterBaseURL is the base under which OpenRouter serves an
// Anthropic-compatible Messages endpoint: POST {base}/v1/messages, the same
// request and response bytes, and the same usage block that the proxy meters
// on. It accepts the x-api-key header and ignores anthropic-version, so the
// AnthropicProvider reaches it unchanged — only the base URL and the model ids
// differ, and those carry a vendor prefix ("anthropic/claude-haiku-4.5").
// Verified against OpenRouter's documentation on 2026-09-06.
const OpenRouterBaseURL = "https://openrouter.ai/api"

// AnthropicProvider forwards Messages-API calls to Anthropic, or to any
// provider that speaks the same wire format (see OpenRouterBaseURL). It is the
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
		Model     string          `json:"model"`
		MaxTokens int64           `json:"max_tokens"`
		System    json.RawMessage `json:"system"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
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
	content := []map[string]any{{"type": "text", "text": text}}
	stop := "end_turn"
	if len(req.Messages) > 0 {
		var heard string
		last := req.Messages[len(req.Messages)-1].Content
		if err := json.Unmarshal(last, &heard); err == nil {
			if answered, ok := stubService(req.System, heard); ok {
				text = answered
				// A service agent with tools calls one when the weightiest
				// word of the message is in the tool's name or description,
				// unless told not to ask. The call is the service's to make;
				// the stub only asks, the way a model does.
				if req.ToolChoice.Type != "none" {
					if want := spec.Salient(heard); want != "" {
						for _, t := range req.Tools {
							if strings.Contains(strings.ToLower(t.Name+" "+t.Description), want) {
								input := map[string]any{}
								for name := range t.InputSchema.Properties {
									input[name] = heard
									break
								}
								text = "Let me ask " + t.Name + "."
								content = []map[string]any{
									{"type": "text", "text": text},
									{"type": "tool_use", "id": fmt.Sprintf("toolu_stub_%016x", digest), "name": t.Name, "input": input},
								}
								stop = "tool_use"
								break
							}
						}
					}
				}
			} else if drafted, ok := stubBuilder(req.System, heard); ok {
				text = drafted
			} else if graded, ok := stubGrade(heard); ok {
				text = graded
			} else if said, ok := stubMind(heard); ok {
				text = said
			}
		} else if result, ok := stubToolResult(last); ok {
			// The service came back with what the tool said. The tool's
			// name is on the tool_use block one message up.
			name := "the tool"
			if len(req.Messages) > 1 {
				var blocks []struct {
					Type, ID, Name string
				}
				json.Unmarshal(req.Messages[len(req.Messages)-2].Content, &blocks)
				for _, b := range blocks {
					if b.Type == "tool_use" && b.ID == result.ToolUseID {
						name = b.Name
					}
				}
			}
			var system string
			json.Unmarshal(req.System, &system)
			text = spec.AnswerTool(system, name, result.Content)
		}
		if stop == "end_turn" {
			content = []map[string]any{{"type": "text", "text": text}}
		}
	}

	resp, err := json.Marshal(map[string]any{
		"id":          fmt.Sprintf("msg_stub_%016x", digest),
		"type":        "message",
		"role":        "assistant",
		"model":       req.Model,
		"content":     content,
		"stop_reason": stop,
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

// stubToolResult reads a tool_result block out of a message whose content
// is a list of blocks, or reports that this was not one.
func stubToolResult(content json.RawMessage) (struct{ ToolUseID, Content string }, bool) {
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return struct{ ToolUseID, Content string }{}, false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return struct{ ToolUseID, Content string }{b.ToolUseID, b.Content}, true
		}
	}
	return struct{ ToolUseID, Content string }{}, false
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

// stubBuilder answers a chat-to-spec call, or reports that this was not
// one. Like stubService it reads the system prompt, where the spec being
// revised is. The draft it writes is a real function of the request — see
// spec.Revise — and always one the service accepts, so the builder can be
// exercised end to end with no model behind it.
func stubBuilder(system json.RawMessage, request string) (string, bool) {
	var s string
	if len(system) == 0 || json.Unmarshal(system, &s) != nil {
		return "", false
	}
	if !strings.HasPrefix(strings.TrimSpace(s), spec.BuilderMarker) {
		return "", false
	}
	return spec.Revise(s, request), true
}

// stubService answers a built agent replying to a message, or reports that
// this was not one. Unlike the judge and the mind, the agent is described in
// the call's system prompt rather than its last message — that is where a
// real model expects a standing instruction — so this is the one branch that
// reads the system field. It is only ever a plain string here; a system given
// as content blocks is not one this package wrote, and falls through.
func stubService(system json.RawMessage, heard string) (string, bool) {
	var s string
	if len(system) == 0 || json.Unmarshal(system, &s) != nil {
		return "", false
	}
	if !strings.HasPrefix(strings.TrimSpace(s), spec.Marker) {
		return "", false
	}
	return spec.Answer(s, heard), true
}
