package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BuilderMarker opens the system prompt of a chat-to-spec call: a person has
// described a change in plain language, and a model is asked to write the
// spec that change produces. The stub recognises it as it recognises Marker,
// and answers with Revise; a trace reader can tell a draft from an agent's
// reply by the first line.
const BuilderMarker = "phylum-builder/1"

// SecSpec labels the block of the builder prompt that carries the current
// spec, as one line of JSON.
const SecSpec = "SPEC"

// ErrNoDraft is returned by ParseDraft when the model's reply carries no
// spec: no JSON object, or one that is not the envelope asked for.
var ErrNoDraft = errors.New("spec: reply carries no draft")

// Draft is what a chat-to-spec call returns: the revised spec and one line
// from the model saying what it changed and why, for the person to read
// before they keep it. The spec is not validated on the way in — the model
// may have made a mistake — and a service validates it after pinning what
// the model may not decide.
type Draft struct {
	Spec Agent  `json:"spec"`
	Note string `json:"note"`
}

// BuilderPrompt renders the system prompt of a chat-to-spec call. The
// current spec rides along as JSON, so the model revises rather than
// invents, and the reply is asked for as JSON alone, so nothing but a spec
// comes back. It may be incomplete — a new agent has no name yet — which is
// why it goes in unvalidated.
//
// How the model is told to revise is the authored choice here, the way
// Prompt is for an agent's voice. The instruction keeps the person's own
// words: the model is asked to change only what the request touches, to
// write persona and rules in the second person the agent will read, and to
// leave the model field alone, which the service pins anyway.
func BuilderPrompt(current Agent) string {
	current.Version = Version
	cur, _ := json.Marshal(current)
	var b strings.Builder
	b.WriteString(BuilderMarker + "\n")
	b.WriteString("You are helping someone build an agent by talking about it. The agent is\n")
	b.WriteString("the JSON document below, and the person's message is a change to make to\n")
	b.WriteString("it. Rewrite the document with that change and nothing else changed: keep\n")
	b.WriteString("what they did not mention, keep their wording where they gave it, and\n")
	b.WriteString("do not invent facts about them or their business. Write persona and\n")
	b.WriteString("rules as instructions the agent will read (\"You are...\", \"Never...\").\n")
	b.WriteString("Each rule is one line. Leave the model field as it is.\n")
	b.WriteString("\nLimits: name up to 64 bytes, persona up to 4096, greeting up to 1024,\n")
	b.WriteString("up to 32 rules of 512 bytes each, max_reply_tokens 1 to 4096.\n")
	b.WriteString("\nReply with one JSON object and no other text:\n")
	b.WriteString(`{"spec": <the whole revised document>, "note": "<one sentence on what you changed>"}` + "\n")
	b.WriteString("\n--- " + SecSpec + " ---\n")
	b.Write(cur)
	b.WriteString("\n\n--- " + secEnd + " ---\n")
	return b.String()
}

// ParseDraft reads the model's reply back into a Draft. Real models wrap
// JSON in fences or a sentence of preamble, so the object is taken from the
// first brace to the last; inside it, unknown keys are refused as Parse
// refuses them. What comes back is a draft, not a spec: Validate it after
// deciding what the model was allowed to change.
func ParseDraft(reply string) (Draft, error) {
	i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if i < 0 || j <= i {
		return Draft{}, ErrNoDraft
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(reply[i : j+1])))
	dec.DisallowUnknownFields()
	var d Draft
	if err := dec.Decode(&d); err != nil {
		return Draft{}, fmt.Errorf("%w: %v", ErrNoDraft, err)
	}
	d.Note = strings.TrimSpace(d.Note)
	return d, nil
}

// Revise is the stub's chat-to-spec: given the builder prompt and the
// person's request, return the JSON reply a model would. It is a visible
// function of both — the request becomes a rule, and an agent with no name
// takes one from the weightiest word in the request — and it always returns
// a draft that validates, so the service's checks pass on the stub for the
// same reason they pass on a real model: the reply obeys the limits. A real
// model reads the request; this one files it.
func Revise(system, request string) string {
	var cur Agent
	json.Unmarshal([]byte(Section(system, SecSpec)), &cur)
	cur.Version = Version

	rule := oneLine(request, MaxRuleBytes)
	var changed []string
	if strings.TrimSpace(cur.Name) == "" {
		if w := oneLine(salient(rule), MaxNameBytes-utf8.UTFMax); w != "" {
			first, n := utf8.DecodeRuneInString(w)
			cur.Name = string(unicode.ToUpper(first)) + w[n:]
		} else {
			cur.Name = "Agent"
		}
		changed = append(changed, "named it "+cur.Name)
	}
	if rule != "" && strings.TrimSpace(cur.Persona) == "" {
		first, n := utf8.DecodeRuneInString(rule)
		cur.Persona = oneLine("You are "+string(unicode.ToLower(first))+strings.TrimSuffix(rule[n:], "."), MaxPersonaBytes) + "."
		changed = append(changed, "wrote a persona from your words")
	}
	if rule != "" {
		if len(cur.Rules) >= MaxRules {
			cur.Rules[MaxRules-1] = rule
			changed = append(changed, "replaced the last rule, the list was full")
		} else {
			cur.Rules = append(cur.Rules, rule)
			changed = append(changed, "added a rule")
		}
	}
	if len(changed) == 0 {
		changed = []string{"changed nothing"}
	}
	out, _ := json.Marshal(Draft{Spec: cur, Note: "Stub: " + strings.Join(changed, ", ") + "."})
	return string(out)
}

// oneLine folds text onto one line, clips it to max bytes on a rune
// boundary, and keeps it from starting with the section syntax: everything
// Validate refuses in a rule, removed rather than refused, because the stub
// must answer every request with a draft that stands.
func oneLine(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	for strings.HasPrefix(text, "--- ") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "--- "))
	}
	if len(text) > max {
		cut := max
		for cut > 0 && !isRuneStart(text[cut]) {
			cut--
		}
		text = strings.TrimRightFunc(text[:cut], unicode.IsSpace)
	}
	return text
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
