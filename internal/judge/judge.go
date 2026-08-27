// Package judge grades open-ended submissions with a model.
//
// A judged bounty has no held-out answer key. The platform states a rubric,
// the agent writes prose, and a model decides whether the prose satisfies the
// rubric. That is a strictly weaker kind of truth than the pure verification
// the ladder rests on — which is exactly why the plan confines judged work to
// the sim track, and why the orchestrator refuses to post one into a ranked
// world at all.
//
// The wire format lives here, alone, because two seams must agree on it byte
// for byte: the judge that writes the prompt, and the stub provider that
// answers it offline. A package that depends on nothing else in the tree can
// be imported by both without a cycle.
package judge

import (
	"strings"
)

// Marker opens every grading prompt. A grader — real model or stub — that
// sees this line first knows it is being asked for a verdict, not for prose.
const Marker = "phylum-judge/1"

// Section labels, in the order Prompt emits them.
const (
	SecRubric     = "RUBRIC"
	SecTask       = "TASK"
	SecSubmission = "SUBMISSION"
)

const secEnd = "END"

// Verdict is a grader's decision about one submission.
type Verdict struct {
	Pass   bool
	Reason string
	// Grader names who decided, for the trace: the model that answered.
	Grader string
}

// Request is everything a grader is shown. The agent's own identity is
// deliberately absent: a judge that knows whose work it is grades the agent.
type Request struct {
	Bounty     string
	Task       string
	Rubric     string
	Submission string
}

// Prompt renders the grading request. The shape is rigid on purpose — the
// stub provider parses it back out with Section, and a real model needs the
// answer format stated plainly enough to obey.
func Prompt(r Request) string {
	var b strings.Builder
	b.WriteString(Marker + "\n")
	b.WriteString("Grade the submission below against the rubric. The rubric is the whole\n")
	b.WriteString("standard: nothing outside it counts for or against. Reply with exactly\n")
	b.WriteString("two lines and nothing else:\n")
	b.WriteString("VERDICT: pass\n")
	b.WriteString("REASON: <one short sentence>\n")
	for _, s := range []struct{ label, body string }{
		{SecRubric, r.Rubric},
		{SecTask, r.Task},
		{SecSubmission, r.Submission},
	} {
		b.WriteString("\n--- " + s.label + " ---\n")
		b.WriteString(s.body + "\n")
	}
	b.WriteString("\n--- " + secEnd + " ---\n")
	return b.String()
}

// Section pulls one labelled block back out of a prompt. Returns "" if the
// block is missing or unterminated.
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

// ParseVerdict reads a grader's reply. A reply that does not carry a verdict
// line is not a "fail" — it is a grader that did not answer the question, and
// the caller must treat it as a platform fault rather than charge the agent
// for the grader's confusion.
func ParseVerdict(text string) (Verdict, bool) {
	var v Verdict
	found := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(strings.ToUpper(line), "VERDICT:"):
			word := strings.ToLower(strings.TrimSpace(line[len("VERDICT:"):]))
			switch word {
			case "pass":
				v.Pass, found = true, true
			case "fail":
				v.Pass, found = false, true
			}
		case strings.HasPrefix(strings.ToUpper(line), "REASON:"):
			v.Reason = strings.TrimSpace(line[len("REASON:"):])
		}
	}
	return v, found
}

// Grade is the stub grader's rule: a submission passes when it contains the
// rubric's text. It is a keyword matcher, and the demo says so out loud — a
// real judge model reads the rubric, this one matches it. What matters for the
// offline demo is that the rule is a genuine function of the submission, so a
// careless agent really does fail and really does eat its costs.
func Grade(rubric, submission string) Verdict {
	want, got := norm(rubric), norm(submission)
	if want != "" && strings.Contains(got, want) {
		return Verdict{Pass: true, Reason: "the submission carries the rubric's required text"}
	}
	return Verdict{Pass: false, Reason: "the submission does not carry the rubric's required text"}
}

// norm collapses formatting noise, the same way bounty.Normalize does for
// keyed answers. Duplicated rather than imported: this package deliberately
// depends on nothing, so the stub provider can use it.
func norm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}
