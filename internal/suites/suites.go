// Package suites imports public benchmark suites as bounty supply.
//
// Generated supply is infinite and unmemorisable: a seed and a tier produce a
// task no model has ever seen. An imported suite is the opposite — a fixed,
// public, finite set of instances, quite possibly sitting in the training
// corpus of every model an agent can call. That makes it valuable (it is the
// work the field already agrees is worth doing) and suspect (a score on it may
// be measuring recall) at the same time.
//
// The plan's resolution is an asterisk rather than a ban: imported bounties
// are ranked, unlike judged ones, but every instance carries its suite name,
// and every suite carries its licence and a contamination note, all the way
// out to the public trace. A reader who wants to discount the score has
// everything needed to do it, and a reader who doesn't still sees the mark.
//
// A suite file is JSONL. The first line is the manifest; every line after it
// is one instance:
//
//	{"name":"...","source":"...","licence":"...","contamination":"..."}
//	{"id":"...","tier":1,"prompt":"...","answer":"...","reference_tokens":40}
package suites

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/metahunmei/dungeon/internal/bounty"
)

// Manifest is a suite's provenance: the four things a reader needs in order to
// know what an asterisked score is worth. All four are required, because an
// unanswered one is exactly the question the asterisk exists to raise.
type Manifest struct {
	// Name is the generator name bounties are posted under.
	Name string `json:"name"`
	// Source says where the instances came from — a paper, a repository, a
	// URL — so the import is traceable to something outside this platform.
	Source string `json:"source"`
	// Licence is the terms the instances are hosted under. Checked by a human
	// before the file is added; recorded here so the check is visible.
	Licence string `json:"licence"`
	// Contamination is an honest sentence about training-corpus exposure. It
	// is a free-text admission rather than a flag because the truth is
	// usually "probably, to an unknown degree" and a boolean would flatten it
	// into a lie in one direction or the other.
	Contamination string `json:"contamination"`
}

// Instance is one imported task. Imported suites are ranked, so an instance
// must be machine-verifiable: it carries an answer key and never a rubric.
type Instance struct {
	ID              string `json:"id"`
	Tier            int    `json:"tier"`
	Prompt          string `json:"prompt"`
	Answer          string `json:"answer"`
	ReferenceTokens int64  `json:"reference_tokens"`
	// Rubric exists on this struct only so that a file carrying one is
	// refused by name rather than silently ignored. A rubric means a model
	// decides the outcome, and a model's opinion may not reach the ladder —
	// so a judged instance in a ranked import is a contradiction, not a task.
	Rubric string `json:"rubric,omitempty"`
}

// Suite is a loaded suite, exposed to the board as an ordinary generator. The
// board cannot tell the difference, which is the point: imported supply moves
// through the same auction, the same ceilings and the same ledger as
// generated supply. Only the label differs.
type Suite struct {
	Manifest
	byTier map[int][]Instance
	tiers  []int // sorted, for deterministic error messages
}

// Name makes a Suite a bounty.Generator.
func (s *Suite) Name() string { return s.Manifest.Name }

// Tiers lists the difficulty tiers this suite can supply, ascending.
func (s *Suite) Tiers() []int { return append([]int(nil), s.tiers...) }

// Len reports how many instances the suite holds.
func (s *Suite) Len() int {
	n := 0
	for _, in := range s.byTier {
		n += len(in)
	}
	return n
}

// Generate picks one instance for (seed, tier). The pick is a pure function of
// its arguments and of the file's line order — no clock, no map iteration —
// so an imported bounty replays exactly like a generated one.
//
// A finite suite means seeds collide: two postings at the same tier will
// eventually draw the same instance. That is a property of importing rather
// than a bug, and the board tolerates it — each posting is a separate bounty
// with its own auction and its own attempt.
func (s *Suite) Generate(seed int64, tier int) (bounty.Task, error) {
	pool := s.byTier[tier]
	if len(pool) == 0 {
		return bounty.Task{}, fmt.Errorf("suite %s: no instances at tier %d (has %v)",
			s.Manifest.Name, tier, s.tiers)
	}
	// Go's % keeps the sign of its left operand, so a negative seed would
	// index backwards off the front of the slice.
	idx := seed % int64(len(pool))
	if idx < 0 {
		idx += int64(len(pool))
	}
	in := pool[idx]
	return bounty.Task{
		Prompt:          in.Prompt,
		Answer:          in.Answer,
		ReferenceTokens: in.ReferenceTokens,
		Suite:           s.Manifest.Name,
	}, nil
}

// Load reads a suite file. Everything it can check, it checks here: a suite
// that is wrong is wrong at import time, where a human is watching, rather
// than mid-episode where it would halt a world.
func Load(path string) (*Suite, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("suite: %w", err)
	}
	defer f.Close()

	s := &Suite{byTier: map[int][]Instance{}}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lineNo, gotManifest := 0, false
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !gotManifest {
			if err := json.Unmarshal([]byte(line), &s.Manifest); err != nil {
				return nil, fmt.Errorf("suite %s line %d: bad manifest: %w", path, lineNo, err)
			}
			if err := s.Manifest.validate(); err != nil {
				return nil, fmt.Errorf("suite %s line %d: %w", path, lineNo, err)
			}
			gotManifest = true
			continue
		}

		var in Instance
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			return nil, fmt.Errorf("suite %s line %d: bad instance: %w", path, lineNo, err)
		}
		if err := in.validate(); err != nil {
			return nil, fmt.Errorf("suite %s line %d: %w", path, lineNo, err)
		}
		if seen[in.ID] {
			return nil, fmt.Errorf("suite %s line %d: duplicate instance id %q", path, lineNo, in.ID)
		}
		seen[in.ID] = true
		s.byTier[in.Tier] = append(s.byTier[in.Tier], in)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("suite %s: %w", path, err)
	}
	if !gotManifest {
		return nil, fmt.Errorf("suite %s: empty file, expected a manifest on line 1", path)
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("suite %s: manifest with no instances", path)
	}

	for tier := range s.byTier {
		s.tiers = append(s.tiers, tier)
	}
	sort.Ints(s.tiers)
	return s, nil
}

func (m Manifest) validate() error {
	switch {
	case m.Name == "":
		return fmt.Errorf("manifest needs a name")
	case m.Source == "":
		return fmt.Errorf("manifest needs a source: an import with no provenance is not importable")
	case m.Licence == "":
		return fmt.Errorf("manifest needs a licence: %s is hosted here and someone owns it", m.Name)
	case m.Contamination == "":
		return fmt.Errorf("manifest needs a contamination note: %s is public, so say what that means for a score on it", m.Name)
	}
	return nil
}

func (in Instance) validate() error {
	switch {
	case in.ID == "":
		return fmt.Errorf("instance needs an id")
	case in.Tier < 1:
		return fmt.Errorf("instance %s: tier must be 1 or more, got %d", in.ID, in.Tier)
	case in.Prompt == "":
		return fmt.Errorf("instance %s: needs a prompt", in.ID)
	// Ordered so the specific diagnosis wins: a rubric-only instance is
	// missing an answer too, but "you brought a rubric" is the useful half.
	case in.Rubric != "":
		return fmt.Errorf("instance %s: carries a rubric — imported suites are ranked, so their instances must be verifiable by key, not decided by a model", in.ID)
	case in.Answer == "":
		return fmt.Errorf("instance %s: needs an answer key", in.ID)
	case in.ReferenceTokens <= 0:
		return fmt.Errorf("instance %s: reference_tokens must be positive; it is what prices the payout", in.ID)
	}
	return nil
}

// LoadDir loads every .jsonl suite in a directory, in filename order. A
// missing directory is not an error: a world with no imported supply is the
// normal case, and the demo's ranked ladder is exactly that world.
func LoadDir(dir string) ([]*Suite, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("suites: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []*Suite
	byName := map[string]string{}
	for _, n := range names {
		s, err := Load(dir + "/" + n)
		if err != nil {
			return nil, err
		}
		// Registering a generator is last-write-wins on a map, so two suites
		// claiming one name would silently retire the first.
		if prev, dup := byName[s.Manifest.Name]; dup {
			return nil, fmt.Errorf("suites: %s and %s both claim the name %q", prev, n, s.Manifest.Name)
		}
		byName[s.Manifest.Name] = n
		out = append(out, s)
	}
	return out, nil
}
