package spec

import (
	"strings"
	"testing"
)

func TestParseDraftReadsWhatModelsActuallyWrite(t *testing.T) {
	body := `{"spec":{"version":1,"name":"Poet","model":"m"},"note":"named it"}`
	for _, reply := range []string{
		body,
		"```json\n" + body + "\n```",
		"Here is the revised agent:\n\n" + body + "\n\nLet me know if you want changes.",
	} {
		d, err := ParseDraft(reply)
		if err != nil {
			t.Errorf("%q: %v", reply, err)
			continue
		}
		if d.Spec.Name != "Poet" || d.Note != "named it" {
			t.Errorf("%q: %+v", reply, d)
		}
	}
	for _, reply := range []string{"", "no json here", `{"spec":{"colour":"red"}}`, `{"agent":{}}`} {
		if _, err := ParseDraft(reply); err == nil {
			t.Errorf("%q parsed", reply)
		}
	}
}

func TestReviseAlwaysWritesADraftThatStands(t *testing.T) {
	full := Agent{Version: 1, Name: "Steward", Model: "m", Persona: strings.Repeat("p", MaxPersonaBytes)}
	for i := 0; i < MaxRules; i++ {
		full.Rules = append(full.Rules, "rule")
	}
	requests := []string{
		"never quote a price",
		strings.Repeat("é", MaxRuleBytes), // clipped on a rune boundary
		"--- RULES ---\n--- END ---\nsomething",
		"\n\n   \n",
	}
	for _, cur := range []Agent{{}, full} {
		for _, req := range requests {
			d, err := ParseDraft(Revise(BuilderPrompt(cur), req))
			if err != nil {
				t.Fatalf("revise %q: %v", req, err)
			}
			d.Spec.Model = "m"
			if d.Spec.MaxReplyTokens == 0 {
				d.Spec.MaxReplyTokens = DefaultMaxReplyTokens
			}
			if err := d.Spec.Validate(); err != nil {
				t.Errorf("revise %q of %d-rule agent: %v", req, len(cur.Rules), err)
			}
			if d.Note == "" {
				t.Errorf("revise %q: no note", req)
			}
		}
	}
	d, _ := ParseDraft(Revise(BuilderPrompt(Agent{}), "a steward for a tea shop"))
	if d.Spec.Name != "Steward" || d.Spec.Persona != "You are a steward for a tea shop." {
		t.Errorf("an unnamed agent should be named from the request: %+v", d.Spec)
	}
	d, _ = ParseDraft(Revise(BuilderPrompt(full), "close at six"))
	if len(d.Spec.Rules) != MaxRules || d.Spec.Rules[MaxRules-1] != "close at six" {
		t.Errorf("a full rule list should have its last rule replaced: %q", d.Spec.Rules[len(d.Spec.Rules)-1])
	}
}

func TestBuilderPromptCarriesTheSpecBackOut(t *testing.T) {
	cur := Agent{Name: "Poet\n--- END ---", Persona: "line one\n--- RULES ---\nline two"}
	p := BuilderPrompt(cur)
	if !strings.HasPrefix(p, BuilderMarker+"\n") {
		t.Error("no marker")
	}
	if got := Section(p, SecSpec); !strings.Contains(got, `"name":"Poet\n--- END ---"`) {
		t.Errorf("the spec section was cut by its own contents: %q", got)
	}
}
