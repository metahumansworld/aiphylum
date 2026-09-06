// Package spec is the agent spec: the one JSON document a built agent is.
//
// Both ways of building an agent — the node editor and the chat that turns
// plain language into an agent — write this document, and the service runtime
// (internal/service) interprets it. No user code runs anywhere: an agent is
// data, and what the platform does with that data is fixed here. That is what
// lets a person with no key and no terminal end up with something reachable
// over HTTP, and it is why the zero-egress container runner is not needed for
// this kind of agent at all.
//
// The package is dependency-free on purpose, like judge and mind: it is a wire
// format shared by two seams. Prompt renders a spec into the system prompt a
// model is given, and Answer is the offline stub's reply to that prompt — a
// dumb reply, but a genuine function of who the agent is and what was just
// said to it, so an agent run on the stub behaves like an agent rather than
// like a hash.
//
// Version 1 is deliberately small: a name, a model, a persona, a greeting, a
// list of rules, a ceiling on reply length, the tools the agent may call —
// each an HTTP endpoint of the owner's, described in words, with the
// parameters the model fills in — and a webhook, the agent's own inbound
// endpoint for events. Memory across conversations and personality
// proper are later additions, and JSON grows without breaking what is here.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Marker opens every system prompt this package renders. The stub recognises
// it the way it recognises the judge's and the mind's, and a trace reader can
// tell a service call from an arena call by the first line of its system
// prompt alone.
const Marker = "phylum-agent/1"

// Version is the spec version this package reads and writes.
const Version = 1

// Section labels in the rendered prompt.
const (
	SecWho   = "WHO"
	SecRules = "RULES"
	secEnd   = "END"
)

// Limits. They bound what one agent can cost per call as much as what it can
// say: every byte of persona and every rule is sent with every message.
const (
	MaxNameBytes     = 64
	MaxPersonaBytes  = 4096
	MaxGreetingBytes = 1024
	MaxRules         = 32
	MaxRuleBytes     = 512

	// DefaultMaxReplyTokens is the reply ceiling a spec gets when it names
	// none. Short, because the ceiling is what the proxy reserves before every
	// call, and a thin wallet is refused on the reservation, not the reply.
	DefaultMaxReplyTokens = 256
	MaxReplyTokensCeiling = 4096

	MaxTools         = 8
	MaxToolDescBytes = 256
	MaxToolURLBytes  = 1024
	MaxParams        = 8

	MaxWebhookBytes = 1024
)

// toolName is what a tool or a parameter may be called: a word a model can
// use as a JSON key without quoting games.
var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

var (
	ErrVersion = errors.New("spec: unsupported version")
	ErrInvalid = errors.New("spec: invalid")
)

// Agent is the document. Field names are the JSON keys the builder writes.
type Agent struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	// Model is the provider's model id as the proxy's price table knows it —
	// for OpenRouter that carries the vendor prefix, "anthropic/claude-haiku-4.5".
	// The service refuses a spec whose model is not on the table at creation,
	// not at first message.
	Model    string   `json:"model"`
	Persona  string   `json:"persona,omitempty"`
	Greeting string   `json:"greeting,omitempty"`
	Rules    []string `json:"rules,omitempty"`
	// MaxReplyTokens caps one reply. It is the max_tokens of every call the
	// agent makes, so it is also the worst case the proxy holds per message.
	MaxReplyTokens int64 `json:"max_reply_tokens,omitempty"`
	// Tools are what the agent may call besides the model: the owner's own
	// HTTP endpoints. The platform makes the call; no user code runs.
	Tools []Tool `json:"tools,omitempty"`
	// Webhook, when present, lets the agent be woken by an event as well
	// as by a message: the owner's systems POST to it and get a reply.
	Webhook *Webhook `json:"webhook,omitempty"`
}

// Webhook is the agent's inbound side. An event is whatever a caller POSTs
// — a form, an order, a build result — and Instruction is what the agent is
// told an event is and what to do with one. There is no secret to check:
// like a message, an event is public and rate-limited, and what it can cost
// the owner is bounded the same way.
type Webhook struct {
	Instruction string `json:"instruction"`
}

// Tool is one HTTP endpoint the agent may call. The model sees the name,
// the description and the parameters, and decides when to call it; the
// platform sends the request — the parameters as a query on a GET, as a JSON
// body on a POST — and hands the response back to the model as text. A
// tool carries no secret: the spec is stored and shown as it is, so
// anything the endpoint needs to know the caller goes in the URL itself.
type Tool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	URL         string  `json:"url"`
	Method      string  `json:"method,omitempty"` // GET (the default) or POST
	Params      []Param `json:"params,omitempty"`
}

// Param is one thing the model fills in when it calls a tool. Every
// parameter is a string and every one is optional: the description is what
// tells the model what to write, and the endpoint is what decides whether
// what it wrote will do.
type Param struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Parse reads a spec from JSON. Unknown keys are refused rather than dropped:
// a builder that writes a field this version does not read has made a
// mistake, and silently losing it would be the worst way to find out. A
// missing reply ceiling takes the default.
func Parse(data []byte) (Agent, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var a Agent
	if err := dec.Decode(&a); err != nil {
		return Agent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if a.MaxReplyTokens == 0 {
		a.MaxReplyTokens = DefaultMaxReplyTokens
	}
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// Validate reports the first thing wrong with a spec, in words a builder can
// show the person who wrote it.
func (a Agent) Validate() error {
	if a.Version != Version {
		return fmt.Errorf("%w: %d (this platform reads version %d)", ErrVersion, a.Version, Version)
	}
	name := strings.TrimSpace(a.Name)
	switch {
	case name == "":
		return fmt.Errorf("%w: the agent needs a name", ErrInvalid)
	case len(name) > MaxNameBytes:
		return fmt.Errorf("%w: name is over %d bytes", ErrInvalid, MaxNameBytes)
	case strings.ContainsAny(name, "\n\r"):
		return fmt.Errorf("%w: name must be one line", ErrInvalid)
	}
	if strings.TrimSpace(a.Model) == "" {
		return fmt.Errorf("%w: the agent needs a model", ErrInvalid)
	}
	if len(a.Persona) > MaxPersonaBytes {
		return fmt.Errorf("%w: persona is over %d bytes", ErrInvalid, MaxPersonaBytes)
	}
	if len(a.Greeting) > MaxGreetingBytes {
		return fmt.Errorf("%w: greeting is over %d bytes", ErrInvalid, MaxGreetingBytes)
	}
	if len(a.Rules) > MaxRules {
		return fmt.Errorf("%w: more than %d rules", ErrInvalid, MaxRules)
	}
	for i, r := range a.Rules {
		switch {
		case strings.TrimSpace(r) == "":
			return fmt.Errorf("%w: rule %d is empty", ErrInvalid, i+1)
		case len(r) > MaxRuleBytes:
			return fmt.Errorf("%w: rule %d is over %d bytes", ErrInvalid, i+1, MaxRuleBytes)
		case strings.ContainsAny(r, "\n\r"):
			return fmt.Errorf("%w: rule %d must be one line", ErrInvalid, i+1)
		}
	}
	if a.MaxReplyTokens < 1 || a.MaxReplyTokens > MaxReplyTokensCeiling {
		return fmt.Errorf("%w: max_reply_tokens must be 1..%d", ErrInvalid, MaxReplyTokensCeiling)
	}
	if len(a.Tools) > MaxTools {
		return fmt.Errorf("%w: more than %d tools", ErrInvalid, MaxTools)
	}
	names := map[string]bool{}
	for i, t := range a.Tools {
		if err := t.validate(); err != nil {
			return fmt.Errorf("%w: tool %d: %v", ErrInvalid, i+1, err)
		}
		if names[t.Name] {
			return fmt.Errorf("%w: two tools named %q", ErrInvalid, t.Name)
		}
		names[t.Name] = true
	}
	if h := a.Webhook; h != nil {
		switch {
		case strings.TrimSpace(h.Instruction) == "":
			return fmt.Errorf("%w: the webhook needs an instruction; it is what the agent does with an event", ErrInvalid)
		case len(h.Instruction) > MaxWebhookBytes:
			return fmt.Errorf("%w: webhook instruction is over %d bytes", ErrInvalid, MaxWebhookBytes)
		}
	}
	// The section syntax is the one thing a persona or rule could counterfeit:
	// a line that reads "--- RULES ---" inside the persona would end the WHO
	// block early on the way back out. Refuse it rather than escape it, so the
	// rendered prompt stays readable and Section stays simple.
	for _, text := range append([]string{a.Persona}, a.Rules...) {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--- ") {
				return fmt.Errorf("%w: a line may not begin with \"--- \"", ErrInvalid)
			}
		}
	}
	return nil
}

// validate is the syntactic half of what a tool must be. Whether the URL
// may be called at all — the scheme, and where the host resolves to — is
// the runtime's policy, decided where the call is made.
func (t Tool) validate() error {
	switch {
	case !toolName.MatchString(t.Name):
		return errors.New("name must be a-z, 0-9 and _, up to 32, starting with a letter")
	case strings.TrimSpace(t.Description) == "":
		return errors.New("needs a description; it is how the model knows when to call it")
	case len(t.Description) > MaxToolDescBytes:
		return fmt.Errorf("description is over %d bytes", MaxToolDescBytes)
	case len(t.URL) > MaxToolURLBytes:
		return fmt.Errorf("url is over %d bytes", MaxToolURLBytes)
	case t.Method != "" && t.Method != "GET" && t.Method != "POST":
		return errors.New("method must be GET or POST")
	case len(t.Params) > MaxParams:
		return fmt.Errorf("more than %d params", MaxParams)
	}
	u, err := url.Parse(t.URL)
	switch {
	case err != nil:
		return fmt.Errorf("url: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("url must be http:// or https://")
	case u.Hostname() == "":
		return errors.New("url needs a host")
	case u.User != nil:
		return errors.New("url may not carry a user or password")
	}
	seen := map[string]bool{}
	for i, p := range t.Params {
		switch {
		case !toolName.MatchString(p.Name):
			return fmt.Errorf("param %d: name must be a-z, 0-9 and _, up to 32, starting with a letter", i+1)
		case len(p.Description) > MaxToolDescBytes:
			return fmt.Errorf("param %d: description is over %d bytes", i+1, MaxToolDescBytes)
		case seen[p.Name]:
			return fmt.Errorf("two params named %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// Prompt renders the spec into the system prompt every call carries. The
// shape is rigid on purpose — Answer parses it straight back out, so the
// format is exercised in both directions every time the stub replies.
//
// How the persona and rules are phrased to the model is the one authored
// choice in this file, and it is where a built agent's quality will be won or
// lost once real models are behind it. Version 1 keeps it plain: say who the
// agent is, hand over the rules verbatim, and ask for the agent's own voice.
func Prompt(a Agent) string {
	var b strings.Builder
	b.WriteString(Marker + "\n")
	b.WriteString("You are the agent described below, in conversation with someone who has\n")
	b.WriteString("written to you. Answer as that agent, in its own voice, and stay inside\n")
	b.WriteString("its rules. Keep each reply short: it is cut off past its ceiling.\n")

	rules := make([]string, 0, len(a.Rules))
	for _, r := range a.Rules {
		rules = append(rules, "- "+strings.TrimSpace(r))
	}
	for _, s := range []struct{ label, body string }{
		{SecWho, strings.TrimSpace(strings.TrimSpace(a.Name) + "\n" + strings.TrimSpace(a.Persona))},
		{SecRules, strings.Join(rules, "\n")},
	} {
		b.WriteString("\n--- " + s.label + " ---\n")
		b.WriteString(s.body + "\n")
	}
	b.WriteString("\n--- " + secEnd + " ---\n")
	return b.String()
}

// Section pulls one labelled block back out of a prompt. Returns "" if the
// block is missing or unterminated. Deliberately identical to judge.Section
// and mind.Section: the three formats are siblings and should stay readable
// as such.
func Section(prompt, label string) string {
	open := "--- " + label + " ---\n"
	i := strings.Index(prompt, open)
	if i < 0 {
		return ""
	}
	rest := prompt[i+len(open):]
	j := strings.Index(rest, "\n--- ")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// Rules reads the rule list back out of a rendered prompt.
func Rules(prompt string) []string {
	var out []string
	for _, l := range strings.Split(Section(prompt, SecRules), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-"))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Answer is the stub's reply to an agent this package described: given the
// system prompt and what the person just wrote, say something that is
// visibly a function of both. It names the agent, echoes the weightiest word
// it was given, and cites the first rule, so a conversation on the stub reads
// as this agent replying to this message — which is all an offline run has to
// prove. A real model reads the persona; this one reads the labels.
func Answer(system, heard string) string {
	name, _, _ := strings.Cut(Section(system, SecWho), "\n")
	name = strings.TrimSpace(name)
	if name == "" {
		name = "The agent"
	}
	topic := Salient(heard)
	if topic == "" {
		return name + " here. Say something and I will answer it."
	}
	reply := name + " here — you asked about " + topic + "."
	if rules := Rules(system); len(rules) > 0 {
		reply += " Rule one: " + rules[0]
	}
	return reply
}

// AnswerTool is the stub's reply once a tool it called has answered: the
// agent, by name, saying what the tool said. Like Answer it is a visible
// function of its inputs, so a round trip through a tool on the stub reads
// as this agent relaying this result.
func AnswerTool(system, tool, result string) string {
	name, _, _ := strings.Cut(Section(system, SecWho), "\n")
	name = strings.TrimSpace(name)
	if name == "" {
		name = "The agent"
	}
	result = strings.Join(strings.Fields(result), " ")
	if len(result) > 200 {
		result = result[:200] + "…"
	}
	return name + " here — I asked " + tool + ", and it said: " + result
}

// Salient is the stub's whole reading of a message: the longest word in it,
// earliest on a tie, with short words skipped so "the" never wins. Exported
// for the stub's tool choice, which reads a message the same way.
func Salient(text string) string {
	best := ""
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.ToLower(strings.Trim(w, "'"))
		if len(w) > 3 && len(w) > len(best) {
			best = w
		}
	}
	return best
}
