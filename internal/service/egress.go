package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/metahumansworld/soscitea/internal/spec"
)

// A tool call is the platform making an HTTP request on an agent's behalf,
// which makes the platform an HTTP client that a stranger, through the
// agent, can aim. What it may be aimed at is decided twice: at Save, where a
// URL must be https to a name that is not this machine or a private one,
// and at dial, where the name is resolved and the address checked before the
// connection is made to that address — not to the name again, so a DNS
// answer that changes between the two cannot slip an internal address past
// the first check. Redirects are not followed at all, for the same reason.
//
// Config.InsecureTools turns the policy off, for a developer's own machine:
// http and private addresses pass, and nothing else changes.
const (
	// ToolTimeout is the whole of one tool call, connect to last byte.
	ToolTimeout = 10 * time.Second
	// MaxToolResultBytes caps what a tool's answer hands the model. Every
	// byte is input on every round after, so it is small.
	MaxToolResultBytes = 8 << 10
	// MaxToolRounds is how many times one message may send the agent to
	// its tools before it must answer in words.
	MaxToolRounds = 4
	// MessageDeadline bounds one message's rounds together: a public caller
	// is waiting on the conversation's lock the whole time.
	MessageDeadline = 90 * time.Second
)

// ErrToolBlocked is a tool URL the policy refuses, at Save or at dial.
var ErrToolBlocked = errors.New("service: tool url is not allowed")

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// blocked reports whether an address is one the platform must not connect
// to on an owner's say-so: this machine, a private network, link-local
// (where cloud metadata lives), or nowhere at all. IPv4 in IPv6 clothing is
// unwrapped first.
func blocked(a netip.Addr) bool {
	if nat64.Contains(a) {
		return true
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a) ||
		(a.Is4() && (a.As4()[0] == 0 || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})))
}

// checkTool is the Save-time half of the policy: what can be told from the
// URL alone, said in words a builder can show.
func checkTool(t spec.Tool, insecure bool) error {
	if insecure {
		return nil
	}
	u, err := url.Parse(t.URL)
	if err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("%w: %s must be https", ErrToolBlocked, t.Name)
	case host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || !strings.Contains(host, "."):
		return fmt.Errorf("%w: %s must reach a public host, not %s", ErrToolBlocked, t.Name, host)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return fmt.Errorf("%w: %s must name its host, not give an address", ErrToolBlocked, t.Name)
	}
	return nil
}

// egressClient is the client tool calls go out on: it resolves a host
// itself, refuses blocked addresses, and dials the address it checked.
func egressClient(insecure bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: ToolTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:               nil, // never the environment's: the check is on the address dialled
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        16,
			IdleConnTimeout:     30 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				for _, a := range addrs {
					if insecure || !blocked(a) {
						return dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
					}
				}
				return nil, fmt.Errorf("%w: %s resolves only to addresses this platform will not call", ErrToolBlocked, host)
			},
		},
	}
}

// callTool makes one tool call and says what came back, as the text the
// model will read, and whether it counts as a failure. Declared params are
// sent and nothing else the model wrote is, as a query on a GET and a JSON
// body of strings on a POST. The answer is the status and the body, clipped.
func (s *Service) callTool(ctx context.Context, agentID string, t spec.Tool, input json.RawMessage) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, ToolTimeout)
	defer cancel()
	var given map[string]any
	json.Unmarshal(input, &given)
	params := map[string]string{}
	for _, p := range t.Params {
		if v, ok := given[p.Name]; ok && v != nil {
			params[p.Name] = fmt.Sprint(v)
		}
	}

	var req *http.Request
	var err error
	if t.Method == "POST" {
		body, _ := json.Marshal(params)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		u, perr := url.Parse(t.URL)
		if perr != nil {
			return "could not call " + t.Name + ": " + perr.Error(), true
		}
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	}
	if err != nil {
		return "could not call " + t.Name + ": " + err.Error(), true
	}
	req.Header.Set("User-Agent", spec.Marker)
	req.Header.Set("X-Phylum-Agent", agentID)
	req.Header.Set("Accept", "application/json, text/*")

	started := time.Now()
	resp, err := s.egress.Do(req)
	if err != nil {
		s.log.Warn("tool call failed", "agent", agentID, "tool", t.Name, "err", err, "ms", time.Since(started).Milliseconds())
		if errors.Is(err, ErrToolBlocked) {
			return t.Name + " is not reachable from here.", true
		}
		return t.Name + " did not answer.", true
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxToolResultBytes+1))
	clipped := len(body) > MaxToolResultBytes
	if clipped {
		body = body[:MaxToolResultBytes]
	}
	s.log.Info("tool call", "agent", agentID, "tool", t.Name, "status", resp.StatusCode, "bytes", len(body), "ms", time.Since(started).Milliseconds())
	text := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, strings.ToValidUTF8(string(body), "�"))
	if clipped {
		text += "\n(clipped)"
	}
	// A redirect is not followed and not a success: the address checked was
	// this one, and the model is told so rather than sent on.
	return text, resp.StatusCode >= 300
}
