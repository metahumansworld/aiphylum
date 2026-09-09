// Package bounty is the board: task supply, award state, and verification.
//
// A bounty is a task instance with a known ground truth held by the platform.
// Verification is a pure function of (hidden answer, submitted answer) — no
// human and no judge model anywhere near it, which is what keeps the ladder's
// number trustworthy.
//
// Judged bounties are the deliberate exception. Open-ended work has no answer
// key, so a model grades it against a hidden rubric — a strictly weaker kind
// of truth. The board keeps them apart at every seam: Verify never says yes to
// one, Resolve refuses to decide one, and the orchestrator will not post one
// into a ranked world at all.
package bounty

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/metahumansworld/soscitea/internal/ledger"
)

// Task is one generated instance. Prompt is what the agent sees; the hidden
// key — Answer or Rubric — never leaves the platform.
//
// Exactly one of Answer and Rubric must be set. A task with an answer is
// machine-verifiable and may be ranked; a task with a rubric is graded by a
// model and may not.
type Task struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
	// Rubric is the grading criterion for an open-ended task. Its presence is
	// what makes a bounty judged.
	Rubric string `json:"rubric"`
	// ReferenceTokens is the measured token cost of the reference solution,
	// which prices the payout as a multiple of it.
	ReferenceTokens int64 `json:"reference_tokens"`
	// Suite names the imported benchmark this instance was drawn from, empty
	// for a generated task. Imported work is ranked — unlike judged work —
	// but it is ranked with an asterisk, because a public benchmark may be
	// sitting in the model's training corpus and the score may be measuring
	// recall. Provenance is the only thing that lets a reader tell, so it
	// travels with the task rather than being reconstructed later.
	Suite string `json:"suite,omitempty"`
}

// Keyed reports whether a task carries exactly one hidden key. Post refuses
// anything else: a task with neither cannot be decided, and a task with both
// leaves two disagreeing notions of correct.
func (t Task) Keyed() bool { return (t.Answer == "") != (t.Rubric == "") }

// Generator produces task instances. Seeded and parameterised: the same seed
// and tier must yield the same task, which is what makes episodes replayable
// and supply infinite.
type Generator interface {
	Name() string
	Generate(seed int64, tier int) (Task, error)
}

// State is a bounty's position in its lifecycle.
type State string

const (
	StateOpen    State = "open"    // on the board, accepting bids
	StateAwarded State = "awarded" // exclusive attempt rights assigned
	StateSolved  State = "solved"  // verified correct; payout made
	StateFailed  State = "failed"  // attempt made and wrong, or ceilings hit
)

// Bounty is one posted task.
type Bounty struct {
	ID        string
	Generator string
	Seed      int64
	Tier      int
	Prompt    string
	answer    string // unexported: the held-out key
	rubric    string // unexported: the held-out grading criterion
	// Judged marks a bounty a model grades rather than a key verifies. Public,
	// because everyone — agent, spectator, ladder — must be able to tell.
	Judged bool
	// Suite is the imported benchmark this instance came from, empty for
	// generated supply. Public for the same reason Judged is: the asterisk
	// belongs to whoever reads the score, not to the platform.
	Suite     string
	MaxPayout ledger.Credits
	State     State
	// Award details, set once won.
	Winner     string // agent ID
	AskedPrice ledger.Credits
	// Attempt ceilings, fixed when the bounty is posted.
	TokenCeiling ledger.Credits
	WallClockSec int
	// Failures counts returned awards: attempts that ended wrong, crashed, or
	// hit a ceiling. Public, so the board shows which bounties have teeth.
	Failures int
}

// PayoutFor prices a bounty's maximum payout from the reference cost and the
// tier. The multiple rises superlinearly with tier (tier^1.6): hard bounties
// pay disproportionately more per reference token than easy ones, which is
// half of what makes sandbagging on cheap certainties a losing strategy (the
// other half is the ladder's volume gates).
func PayoutFor(referenceCost ledger.Credits, tier int) ledger.Credits {
	if tier < 1 {
		tier = 1
	}
	multiple := 3.0 * math.Pow(float64(tier), 1.6)
	return ledger.Credits(float64(referenceCost) * multiple)
}

// Normalize collapses formatting noise before comparison so that verification
// tests answers, not whitespace.
func Normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

// Verify is the pure function the whole ladder rests on. A judged bounty has
// no key to compare against, so it never verifies — its outcome arrives from
// outside, through ResolveJudged.
func (b *Bounty) Verify(submitted string) bool {
	if b.Judged {
		return false
	}
	return Normalize(submitted) == Normalize(b.answer)
}

// AnswerDigest exposes a hash of the bounty's hidden key — the answer, or for
// a judged bounty the rubric — for the public trace: enough for anyone to
// audit post-hoc that the key existed before the attempt, without revealing it
// while the bounty is live.
func (b *Bounty) AnswerDigest() string {
	key := b.answer
	if b.Judged {
		key = b.rubric
	}
	sum := sha256.Sum256([]byte(Normalize(key)))
	return hex.EncodeToString(sum[:8])
}

// Rubric returns the hidden grading criterion. Only the judge may read it, so
// this is deliberately not on any view the agent sees.
func (b *Bounty) Rubric() string { return b.rubric }

var (
	ErrNotOpen     = errors.New("bounty: not open for award")
	ErrNotAwarded  = errors.New("bounty: no award to attempt against")
	ErrUnknown     = errors.New("bounty: no such bounty")
	ErrNoGenerator = errors.New("bounty: no such generator")
	ErrJudged      = errors.New("bounty: judged bounty is decided by a verdict, not a key")
	ErrNotJudged   = errors.New("bounty: keyed bounty is decided by its key, not a verdict")
)

// Board holds the live bounties and the generator registry.
type Board struct {
	mu         sync.Mutex
	generators map[string]Generator
	bounties   map[string]*Bounty
	order      []string // posting order, for deterministic listing
	nextID     int
}

// Resume moves the numbering past the last bounty an earlier process posted
// over the same record, so a restarted world never posts b0001 twice. The
// daemon reads that number from the trace at boot; it is nowhere else.
func (bd *Board) Resume(lastID int) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if lastID > bd.nextID {
		bd.nextID = lastID
	}
}

// LastID is the number of the last bounty posted — the counter a checkpoint
// records so Resume can carry it into the next process.
func (bd *Board) LastID() int {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	return bd.nextID
}

// Restore puts a bounty an earlier process posted back on the board, under
// its own ID and with its failures counted, without posting it again. The
// task is regenerated from the generator and seed exactly as Post made it,
// so the held-out key never has to be written down anywhere: a checkpoint
// carries the recipe, not the answer. Only open bounties are worth
// restoring — a solved or failed one is history the trace already holds.
func (bd *Board) Restore(id, generator string, seed int64, tier int, tokenCeiling ledger.Credits, wallClockSec, failures int) (*Bounty, error) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	if _, dup := bd.bounties[id]; dup {
		return nil, fmt.Errorf("bounty: %s restored twice", id)
	}
	g, ok := bd.generators[generator]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoGenerator, generator)
	}
	task, err := g.Generate(seed, tier)
	if err != nil {
		return nil, fmt.Errorf("generate %s seed %d tier %d: %w", generator, seed, tier, err)
	}
	refCost := ledger.Credits(task.ReferenceTokens * 5)
	b := &Bounty{
		ID: id, Generator: generator, Seed: seed, Tier: tier,
		Prompt: task.Prompt, answer: task.Answer, rubric: task.Rubric,
		Judged: task.Rubric != "", Suite: task.Suite,
		MaxPayout: PayoutFor(refCost, tier), State: StateOpen,
		TokenCeiling: tokenCeiling, WallClockSec: wallClockSec, Failures: failures,
	}
	bd.bounties[id] = b
	bd.order = append(bd.order, id)
	return b, nil
}

func NewBoard() *Board {
	return &Board{
		generators: map[string]Generator{},
		bounties:   map[string]*Bounty{},
	}
}

// RegisterGenerator adds a task source to the board.
func (bd *Board) RegisterGenerator(g Generator) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	bd.generators[g.Name()] = g
}

// Generators lists the registered task sources, sorted by name — the stable
// order a caller needs to derive a deterministic posting plan from the
// registry rather than from a hard-coded list.
func (bd *Board) Generators() []string {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	out := make([]string, 0, len(bd.generators))
	for name := range bd.generators {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Probe generates a task from a registered generator without posting it —
// how a caller planning an episode can learn, ahead of time, whether a
// generator's supply is judged and would be refused in a ranked world.
func (bd *Board) Probe(generator string, seed int64, tier int) (Task, error) {
	bd.mu.Lock()
	g, ok := bd.generators[generator]
	bd.mu.Unlock()
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrNoGenerator, generator)
	}
	return g.Generate(seed, tier)
}

// Post generates one task and puts it on the board.
func (bd *Board) Post(generator string, seed int64, tier int, tokenCeiling ledger.Credits, wallClockSec int) (*Bounty, error) {
	bd.mu.Lock()
	defer bd.mu.Unlock()

	g, ok := bd.generators[generator]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoGenerator, generator)
	}
	task, err := g.Generate(seed, tier)
	if err != nil {
		return nil, fmt.Errorf("generate %s seed %d tier %d: %w", generator, seed, tier, err)
	}
	if !task.Keyed() {
		return nil, fmt.Errorf("generate %s seed %d tier %d: a task needs exactly one of answer and rubric",
			generator, seed, tier)
	}

	bd.nextID++
	// The reference solution's cost in credits: tokens priced at a nominal
	// blended rate. The exact rate matters less than that payouts and spend
	// share a unit; generators report tokens, the board prices them.
	refCost := ledger.Credits(task.ReferenceTokens * 5) // 5 credits/token nominal blend
	b := &Bounty{
		ID:           fmt.Sprintf("b%04d", bd.nextID),
		Generator:    generator,
		Seed:         seed,
		Tier:         tier,
		Prompt:       task.Prompt,
		answer:       task.Answer,
		rubric:       task.Rubric,
		Judged:       task.Rubric != "",
		Suite:        task.Suite,
		MaxPayout:    PayoutFor(refCost, tier),
		State:        StateOpen,
		TokenCeiling: tokenCeiling,
		WallClockSec: wallClockSec,
	}
	bd.bounties[b.ID] = b
	bd.order = append(bd.order, b.ID)
	return b, nil
}

// Open lists bounties currently accepting bids, in posting order.
func (bd *Board) Open() []*Bounty {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	var out []*Bounty
	for _, id := range bd.order {
		if b := bd.bounties[id]; b.State == StateOpen {
			out = append(out, b)
		}
	}
	return out
}

// Get returns a bounty by ID.
func (bd *Board) Get(id string) (*Bounty, error) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b, ok := bd.bounties[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	return b, nil
}

// Award assigns exclusive attempt rights at the winner's asking price.
func (bd *Board) Award(id, agentID string, askedPrice ledger.Credits) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b, ok := bd.bounties[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if b.State != StateOpen {
		return fmt.Errorf("%w: %s is %s", ErrNotOpen, id, b.State)
	}
	b.State = StateAwarded
	b.Winner = agentID
	b.AskedPrice = askedPrice
	return nil
}

// Resolve records the outcome of the winner's one attempt. A correct answer
// solves the bounty; anything else — wrong answer, crash, ceilings — fails it,
// and failure returns it to auction with the award cleared.
func (bd *Board) Resolve(id string, submitted string, attempted bool) (solved bool, err error) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b, ok := bd.bounties[id]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if b.State != StateAwarded {
		return false, fmt.Errorf("%w: %s is %s", ErrNotAwarded, id, b.State)
	}
	if b.Judged {
		return false, fmt.Errorf("%w: %s", ErrJudged, id)
	}
	solved = attempted && b.Verify(submitted)
	b.close(solved)
	return solved, nil
}

// ResolveJudged records the outcome of an attempt at a judged bounty. The
// verdict comes from the judge, so this takes it rather than deciding it —
// the one place in the board where correctness is somebody else's word.
func (bd *Board) ResolveJudged(id string, passed bool) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b, ok := bd.bounties[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if b.State != StateAwarded {
		return fmt.Errorf("%w: %s is %s", ErrNotAwarded, id, b.State)
	}
	if !b.Judged {
		return fmt.Errorf("%w: %s", ErrNotJudged, id)
	}
	b.close(passed)
	return nil
}

// close applies an attempt's outcome to the bounty. Caller holds the lock.
func (b *Bounty) close(solved bool) {
	if solved {
		b.State = StateSolved
		return
	}
	// One attempt's tokens per bounty: the winner ate its costs, and the
	// bounty goes back on the board for others.
	b.State = StateOpen
	b.Winner = ""
	b.AskedPrice = 0
	b.Failures++
}

// Void unwinds an award because the platform failed, not the agent. The
// bounty returns to auction with the award cleared and — the point — no
// failure counted: a proxy outage must not show up publicly as an agent
// mistake. Pairs with ledger.Release on the money side; together they are the
// refunded-and-voided half of failure attribution.
func (bd *Board) Void(id string) error {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	b, ok := bd.bounties[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if b.State != StateAwarded {
		return fmt.Errorf("%w: %s is %s", ErrNotAwarded, id, b.State)
	}
	b.State = StateOpen
	b.Winner = ""
	b.AskedPrice = 0
	return nil
}
