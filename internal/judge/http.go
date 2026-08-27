// The grader that actually spends money: one metered call through the same
// proxy every agent uses, on a wallet the platform funds.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HTTP grades by calling a model through the metering proxy. It holds no key
// and no wallet — only a bearer token the orchestrator authorizes per call —
// so a judge call is metered, priced and refused on exactly the same terms as
// an agent's, and shows up in the trace beside them.
type HTTP struct {
	Base      string // proxy base URL
	Model     string
	MaxTokens int64
	Client    *http.Client
}

// Decide grades one submission. Every error it returns is a platform fault:
// the grader is the platform's instrument, so a grader that cannot answer is
// the platform failing to judge, never the agent failing to solve.
func (h *HTTP) Decide(ctx context.Context, token string, r Request) (Verdict, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	maxTokens := h.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 128
	}

	body, err := json.Marshal(map[string]any{
		"model":      h.Model,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": Prompt(r)},
		},
	})
	if err != nil {
		return Verdict{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Verdict{}, fmt.Errorf("judge call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, fmt.Errorf("judge call: reading reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// A refused judge call means the platform could not afford to judge —
		// its own failure, and attributed as one.
		return Verdict{}, fmt.Errorf("judge call: proxy returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}

	var reply struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Verdict{}, fmt.Errorf("judge call: unreadable reply: %w", err)
	}
	var text string
	for _, c := range reply.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	v, ok := ParseVerdict(text)
	if !ok {
		return Verdict{}, fmt.Errorf("judge call: grader gave no verdict line: %q", text)
	}
	v.Grader = reply.Model
	if v.Grader == "" {
		v.Grader = h.Model
	}
	return v, nil
}
