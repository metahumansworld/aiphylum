package suites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodManifest = `{"name":"t","source":"a paper","licence":"CC0-1.0","contamination":"probably in every corpus"}`

func write(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func inst(id string, tier int, answer string) string {
	return `{"id":"` + id + `","tier":` + string(rune('0'+tier)) +
		`,"prompt":"p ` + id + `","answer":"` + answer + `","reference_tokens":10}`
}

// The point of a suite is that it behaves like a generator: same seed, same
// tier, same task, forever. Without that an imported bounty could not be
// replayed, and replay exactness is the claim the whole trace rests on.
func TestGenerateIsDeterministicAndSelectsWithinTier(t *testing.T) {
	path := write(t, goodManifest,
		inst("a", 1, "1"), inst("b", 1, "2"), inst("c", 1, "3"),
		inst("d", 2, "4"))
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 4 {
		t.Fatalf("len = %d, want 4", s.Len())
	}
	if got := s.Tiers(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("tiers = %v, want [1 2]", got)
	}

	for _, seed := range []int64{0, 1, 2, 3, 99, 1 << 40} {
		first, err := s.Generate(seed, 1)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			again, err := s.Generate(seed, 1)
			if err != nil {
				t.Fatal(err)
			}
			if again != first {
				t.Fatalf("seed %d drew %+v then %+v — not deterministic", seed, first, again)
			}
		}
		// A tier-1 seed must never reach the tier-2 instance: difficulty is
		// what prices the payout, so drawing across tiers would mean paying
		// tier-2 rates for tier-1 work.
		if first.Answer == "4" {
			t.Fatalf("seed %d at tier 1 drew the tier-2 instance", seed)
		}
		if first.Suite != "t" {
			t.Errorf("task suite = %q, want t — provenance did not travel with the task", first.Suite)
		}
	}

	// Every instance in the tier is reachable; the pick is not a constant.
	seen := map[string]bool{}
	for seed := int64(0); seed < 3; seed++ {
		task, err := s.Generate(seed, 1)
		if err != nil {
			t.Fatal(err)
		}
		seen[task.Answer] = true
	}
	if len(seen) != 3 {
		t.Errorf("three seeds drew %d distinct instances, want 3", len(seen))
	}
}

// Go's % keeps the sign of its left operand, so a negative seed would index
// backwards off the front of the slice and panic. Seeds are int64 and come
// from episode plans, so this is reachable by configuration, not by attack.
func TestGenerateHandlesNegativeSeeds(t *testing.T) {
	s, err := Load(write(t, goodManifest, inst("a", 1, "1"), inst("b", 1, "2")))
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []int64{-1, -2, -7, -1 << 40} {
		task, err := s.Generate(seed, 1)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if task.Prompt == "" {
			t.Fatalf("seed %d drew an empty task", seed)
		}
	}
}

// postBounty halts an episode on a generator error, so a tier the suite cannot
// supply must fail loudly and name what it does have — the operator's next
// move is either to fix the deck or to extend the suite.
func TestGenerateRefusesAnAbsentTier(t *testing.T) {
	s, err := Load(write(t, goodManifest, inst("a", 1, "1"), inst("b", 3, "2")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Generate(1, 2)
	if err == nil {
		t.Fatal("tier 2 generated from a suite holding tiers 1 and 3")
	}
	if !strings.Contains(err.Error(), "[1 3]") {
		t.Errorf("error %q does not name the available tiers", err)
	}
}

// The manifest fields are the asterisk's content. A suite that skips one is a
// suite whose score cannot be weighed, so it does not load at all.
func TestLoadRequiresFullProvenance(t *testing.T) {
	cases := map[string]string{
		"name":          `{"source":"s","licence":"l","contamination":"c"}`,
		"source":        `{"name":"n","licence":"l","contamination":"c"}`,
		"licence":       `{"name":"n","source":"s","contamination":"c"}`,
		"contamination": `{"name":"n","source":"s","licence":"l"}`,
	}
	for missing, manifest := range cases {
		_, err := Load(write(t, manifest, inst("a", 1, "1")))
		if err == nil {
			t.Errorf("manifest without a %s loaded", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("manifest without a %s failed with %q, which does not say which field", missing, err)
		}
	}
}

// Imported suites are ranked. A rubric means a model decides the outcome, and
// a model's opinion may never reach the ladder — so the two cannot combine,
// and the refusal has to happen here rather than at posting time.
func TestLoadRefusesAJudgedInstance(t *testing.T) {
	_, err := Load(write(t, goodManifest,
		`{"id":"a","tier":1,"prompt":"p","rubric":"be nice","reference_tokens":10}`))
	if err == nil {
		t.Fatal("an instance with a rubric loaded into a ranked suite")
	}
	if !strings.Contains(err.Error(), "rubric") {
		t.Errorf("error %q does not mention the rubric", err)
	}

	// An instance with a rubric alongside an answer is the same problem
	// wearing a disguise: two disagreeing notions of correct.
	_, err = Load(write(t, goodManifest,
		`{"id":"a","tier":1,"prompt":"p","answer":"1","rubric":"be nice","reference_tokens":10}`))
	if err == nil {
		t.Fatal("an instance with both an answer and a rubric loaded")
	}
}

func TestLoadRejectsMalformedInstances(t *testing.T) {
	cases := map[string]string{
		"no id":       `{"tier":1,"prompt":"p","answer":"1","reference_tokens":10}`,
		"no tier":     `{"id":"a","prompt":"p","answer":"1","reference_tokens":10}`,
		"no prompt":   `{"id":"a","tier":1,"answer":"1","reference_tokens":10}`,
		"no answer":   `{"id":"a","tier":1,"prompt":"p","reference_tokens":10}`,
		"no tokens":   `{"id":"a","tier":1,"prompt":"p","answer":"1"}`,
		"not json":    `{"id":`,
		"no instance": "",
	}
	for name, line := range cases {
		lines := []string{goodManifest}
		if line != "" {
			lines = append(lines, line)
		}
		if _, err := Load(write(t, lines...)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}

	// Two instances under one id would make a suite's own record ambiguous.
	if _, err := Load(write(t, goodManifest, inst("a", 1, "1"), inst("a", 2, "2"))); err == nil {
		t.Error("duplicate instance ids loaded")
	}
}

// RegisterGenerator is last-write-wins on a map, so two suites claiming one
// name would silently retire the first — and its instances would keep being
// attributed to it in the manifest note. Catch it at load.
func TestLoadDirRefusesDuplicateSuiteNames(t *testing.T) {
	dir := t.TempDir()
	body := goodManifest + "\n" + inst("a", 1, "1") + "\n"
	for _, name := range []string{"one.jsonl", "two.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("two suites named t loaded together")
	}

	// A world with no imported supply is the normal case, not an error.
	got, err := LoadDir(filepath.Join(dir, "nope"))
	if err != nil || got != nil {
		t.Errorf("LoadDir on a missing directory = %v, %v; want nil, nil", got, err)
	}
}

// The shipped suite is the worked example of the format, so it has to load,
// and its instances have to be solvable by the demo agents — which means the
// prompt shape the generators use.
func TestShippedSampleSuiteLoads(t *testing.T) {
	suites, err := LoadDir("../../generators/suites")
	if err != nil {
		t.Fatal(err)
	}
	if len(suites) != 1 {
		t.Fatalf("loaded %d suites from generators/suites, want 1", len(suites))
	}
	s := suites[0]
	if s.Name() != "precedence-handbook" {
		t.Errorf("suite name = %q", s.Name())
	}
	if got := s.Tiers(); len(got) != 3 {
		t.Errorf("tiers = %v, want three of them", got)
	}
	for _, tier := range s.Tiers() {
		task, err := s.Generate(0, tier)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(task.Prompt, "[arith] ") || !strings.Contains(task.Prompt, "\nspec: ") {
			t.Errorf("tier %d prompt is not in the shape the demo agents parse: %q", tier, task.Prompt)
		}
	}
}
