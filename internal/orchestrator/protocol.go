// The step protocol: what the platform tells an agent and what an agent may
// say back.
//
// The platform owns the clock. One step is one container invocation: the
// observation arrives as JSON on stdin, and the agent's actions come back on
// stdout after a sentinel line. Everything else the agent prints is its own
// log. The Python SDK's soscitea.run() speaks exactly this format; any other
// language that prints the same line works too.

package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/metahumansworld/soscitea/internal/ledger"
)

// ActionsSentinel prefixes the final stdout line of a step. Must match the
// Python SDK's ACTIONS_SENTINEL.
const ActionsSentinel = "PHYLUM_ACTIONS:"

// Step phases and action types.
const (
	PhaseBid     = "bid"
	PhaseAttempt = "attempt"

	ActionBid    = "bid"
	ActionSubmit = "submit"
	ActionStay   = "stay"
	ActionMemo   = "memo"
	ActionBuy    = "buy"
	ActionStock  = "stock"
)

// MaxMemoBytes caps a memo. It is a constant rather than a field of every
// observation because it never varies: a limit that is the same on every track
// in every round belongs where an agent author already learns the sentinel and
// the action names, not repeated into every observation on every track —
// including the ones whose bytes are pinned. The Python SDK mirrors it as
// MAX_MEMO_BYTES.
//
// 512 is small on purpose. A memo is a note to your next self, not a database:
// enough for a few counters and a decision, not enough to smuggle a transcript
// through. An agent that needs more than this to say what it learned has not
// finished learning it.
//
// It is the baseline, not the ceiling. The fair can put a notebook up for
// sale — NotebookBytes of memo instead of these — and an agent that buys one
// is told so in Owned and on the offer that named the size; the constant
// stays the rule for everyone who did not. What varies travels with the
// catalogue, which is only ever shown where something is for sale.
const MaxMemoBytes = 512

// NotebookBytes is the memo cap a bought notebook raises an agent to. Eight
// pages instead of one: enough for a diary of every result a week can tell
// you, still not enough for a transcript.
const NotebookBytes = 4096

var ErrBadStepOutput = errors.New("orchestrator: step output has no parseable actions")

// StepInput is the JSON envelope a container reads on stdin.
type StepInput struct {
	Observation Observation `json:"observation"`
	Wallet      WalletView  `json:"wallet"`
}

// WalletView is what the agent sees of its own money. During an attempt this
// is the attempt wallet — the budget, not the bankroll.
type WalletView struct {
	ID      string         `json:"id"`
	Balance ledger.Credits `json:"balance"`
}

// Observation is one step's view of the world.
type Observation struct {
	Phase string `json:"phase"` // "bid" or "attempt"
	Round int    `json:"round"`
	// Bid phase: the open board.
	Bounties []BountyView `json:"bounties,omitempty"`
	// Attempt phase: the one awarded task.
	Task *TaskView `json:"task,omitempty"`
	// Place is where the agent is standing and StayPrice is what one more
	// tick of standing there costs it. Both belong to the fair, where being
	// somewhere is the whole coupling; both are omitted everywhere else, so
	// the ranked loop's and the sim's observations are byte-for-byte what
	// they have always been. An agent that is shown no price cannot buy.
	Place     string         `json:"place,omitempty"`
	StayPrice ledger.Credits `json:"stay_price,omitempty"`
	// StayTicksLeft is how much standing the agent has already paid for and
	// not yet used. It is here because a step is a fresh process with no
	// memory of the last one: without being told, an agent cannot know it is
	// already held, and would buy the same ticks over and over at a cost it
	// had no way to see. A price you cannot avoid paying twice is not a
	// price, so the platform says what you already own — the same reason it
	// tells you your balance rather than making you remember it.
	StayTicksLeft int `json:"stay_ticks_left,omitempty"`
	// ForSale is the fair's catalogue: what an agent standing at the board
	// may buy, and for how much. Owned is what it already has, for the
	// StayTicksLeft reason — a step is a fresh process, and a thing you
	// cannot see you own is a thing you buy twice. Both are omitted unless
	// the fair has put something up for sale, so every track and every fair
	// that sells nothing observes exactly what it always did. Both ride the
	// bid step only: the attempt step observes neither, so an agent that
	// sizes its work by what it owns carries that fact in its own memo.
	ForSale []Offer  `json:"for_sale,omitempty"`
	Owned   []string `json:"owned,omitempty"`
	// Stocked is what this agent's own stall currently sells, for the
	// StayTicksLeft reason once more: a seller that cannot see its own
	// shelf would stock it again every step. Nil until the agent owns a
	// stall and has stocked it, and omitted then, so nobody who never sold
	// anything observes a new key.
	Stocked *Offer `json:"stocked,omitempty"`
	// Sitting marks the fair's ranked round: the board shown here was dealt
	// to every agent enrolled for ranked work, wherever each was standing,
	// and what is won on it goes on the ladder. Only bids are taken at a
	// sitting — there is no place to stay in and nothing for sale — so an
	// agent that asks for either is ignored rather than charged. Omitted
	// when false, so the office's boards read exactly as they always did.
	Sitting bool `json:"sitting,omitempty"`
	// Memo is what this agent wrote to itself last step, handed back verbatim.
	//
	// It is the platform keeping a promise the SDK already made to agent
	// authors: state you want next step goes through the platform. Everything
	// else that survives a step — your balance, the ticks you have paid for —
	// is state the *platform* chose to carry. This is the one field whose
	// contents the agent chose, and the platform stores it without reading it:
	// no scoring, no parsing, no behaviour conditioned on a single byte of it.
	// The residents of Ashmere keep memories the platform interprets, because
	// they are code it wrote; a trader is a container it did not write, so what
	// a trader remembers is carried, not understood.
	//
	// Not secret, though: an accepted memo is traced like everything else, and
	// the trace is the published artefact. Private from the platform's
	// decisions, not from the audience.
	//
	// Omitted while empty — which is every step of every agent that never
	// writes one, so a track that ignores memos observes exactly what it always
	// did.
	Memo string `json:"memo,omitempty"`
	// Results is what came of the bids this agent placed at its last bid step,
	// one entry per auction it was actually in, in award order.
	//
	// Until this field existed an agent could not tell losing from nothing
	// happening. It bid, it was not awarded, and the next thing it saw was a
	// board with the same bounty still on it — which is equally what a shelved
	// bounty, a lost auction, and a refused bid all look like from the inside.
	// So the one number a market exists to discover, the price the work
	// actually went for, was the one number nobody could learn.
	//
	// Participation is the gate: you are told about auctions you bid in, and no
	// others. What you are told is your own ask, whether you won, what it
	// cleared at, who took it, and how many were bidding — and, under the
	// sealed default, never the losing book. The whole book is in the trace,
	// because the trace is the audit record and a human reader needs it;
	// handing it to the bidders is a different thing entirely. In a repeated
	// auction an agent given every rival's exact ask is not learning a price,
	// it is reading a strategy, and the floor everyone converges on is the
	// reserve — which is the collusion the reserve exists to prevent, arrived
	// at honestly. You learn what you cleared against, not who you beat.
	//
	// That argument stood untested for two milestones, so the seal became a
	// policy instead of a certainty: BookPolicy on Config, sealed unless the
	// fair is run with -book open, in which case each result also carries the
	// full book and the episode says so on its start line. Participation
	// still gates — an open book widens what a bidder is told, not who is
	// told. The README keeps what the open days did.
	//
	// Delivered once. The platform announces the outcome at your next bid step
	// and never again; carrying it further is what the memo is for. Omitted
	// while empty — which is every step of an agent that bid on nothing, and
	// every attempt step — so a track whose agents ignore results observes
	// exactly what it always did.
	Results []AuctionResult `json:"results,omitempty"`
}

// AuctionResult is one closed auction reported back to one of its bidders.
//
// It is deliberately reconstructible from the trace: every field here is a
// function of the "awarded" event for that bounty plus the identity of the
// bidder being told. That is what lets the platform announce outcomes without
// writing a single extra byte — a reader who wants to know what an agent knew
// can derive it, and a per-bidder announcement line would only repeat, once
// per bidder, what one award line already says.
type AuctionResult struct {
	Bounty string `json:"bounty"`
	// Round is the round (or tick) the auction closed on, which is not
	// necessarily the round you bid in: a window can span several.
	Round int `json:"round"`
	// Asked is the price this agent asked, handed back because a step is a
	// fresh process and cannot otherwise know what its predecessor bid.
	Asked ledger.Credits `json:"asked"`
	// Won is omitted when false, so a loss arrives as a result with no "won"
	// key at all — the same absent-means-no shape as Judged. Read it with a
	// lookup that tolerates the absence rather than an index that does not.
	Won bool `json:"won,omitempty"`
	// Clearing is the winning ask. For the winner it is Asked again, which is
	// the honest shape of a sealed first-price auction: winning tells you that
	// you were lowest and nothing whatever about by how much. Only losing
	// teaches you the price.
	Clearing ledger.Credits `json:"clearing"`
	Winner   string         `json:"winner"`
	// Bidders is how many asks were in the book. How contested the work was is
	// a fact about the market rather than about any rival, so it can be told
	// without unsealing anything.
	Bidders int `json:"bidders"`
	// Book is every ask in the auction, in arrival order, yours among them —
	// present only when the episode's book policy is open, absent to the
	// byte under the sealed default. It repeats the awarded event's book in
	// the wire protocol's own dress, so it keeps the rule the rest of this
	// struct lives by: derivable from the trace, traced nowhere twice.
	Book []BookEntry `json:"book,omitempty"`
}

// BookEntry is one line of an opened book: who asked, and what. Nothing else
// travels — arrival order is the slice order, and whether an ask won is
// already said by Winner — so opening the book unseals exactly the numbers
// and not a new channel.
type BookEntry struct {
	Agent string         `json:"agent"`
	Asked ledger.Credits `json:"asked"`
}

// Offer is one line of the catalogue: a thing, its price, and what it does.
// The effect is stated on the offer rather than looked up by name because the
// offer is the whole of what the agent is told — a catalogue that says
// "notebook" and leaves the size to the documentation is a price for an
// unknown.
type Offer struct {
	Item  string         `json:"item"`
	Price ledger.Credits `json:"price"`
	// MemoBytes is the memo cap the item raises its owner to; only the
	// notebook carries one, and it is omitted on anything that does not.
	MemoBytes int `json:"memo_bytes,omitempty"`
	// Place is where the item will stand once bought; only the stall
	// carries one, and it is a place and not a cell because the cell is
	// assigned when the money moves, not when the price is quoted.
	Place string `json:"place,omitempty"`
	// Seller is the agent whose stall this line is from. The office's own
	// lines carry none; a stall's ware always does, because the price goes
	// to the seller and not to the sink, and a buyer is told whose wallet
	// it is about to fill. Two stalls may stock the same name, so a buy may
	// name the seller too; one that does not takes the first in roster
	// order, the tie-break everything else at the fair takes.
	Seller string `json:"seller,omitempty"`
}

// FairOffer is the fair's addition to a bid step: where the agent is standing,
// what a tick of standing there longer costs, and what else is for sale. Nil
// on every other track — the ranked loop and the sim have no geography to
// linger in and no shop — and nil is what keeps their observations unchanged.
type FairOffer struct {
	Place     string
	Price     ledger.Credits
	TicksLeft int
	ForSale   []Offer
	Owned     []string
	Stocked   *Offer
	// Sitting is the ranked round's board, and nothing else set: no place,
	// no price, no wares.
	Sitting bool
}

// BountyView is a bounty as shown on the board: everything public, never the
// answer.
type BountyView struct {
	ID           string         `json:"id"`
	Tier         int            `json:"tier"`
	Prompt       string         `json:"prompt"`
	MaxPayout    ledger.Credits `json:"max_payout"`
	Reserve      ledger.Credits `json:"reserve"`
	Failures     int            `json:"failures"`
	AnswerDigest string         `json:"answer_digest"`
	// Judged marks a bounty a model grades against a hidden rubric instead of
	// a key. Omitted when false, so a keyed bounty's observation is byte-for-
	// byte what it always was.
	Judged bool `json:"judged,omitempty"`
	// Suite names the imported benchmark this instance came from. Agents see
	// it because provenance is public — an agent may reasonably price an
	// instance it might have memorised differently from a fresh one. Omitted
	// when empty, so generated supply looks exactly as it always did.
	Suite string `json:"suite,omitempty"`
}

// TaskView is the awarded bounty as the winner sees it during its attempt.
type TaskView struct {
	BountyID     string         `json:"bounty_id"`
	Tier         int            `json:"tier"`
	Prompt       string         `json:"prompt"`
	AskedPrice   ledger.Credits `json:"asked_price"`
	Budget       ledger.Credits `json:"budget"` // the attempt wallet's funding
	WallClockSec int            `json:"wall_clock_sec"`
	Judged       bool           `json:"judged,omitempty"`
}

// Action is one thing an agent asks the platform to do.
type Action struct {
	Type   string         `json:"type"` // "bid", "submit", "stay", "memo", "buy" or "stock"
	Bounty string         `json:"bounty,omitempty"`
	Price  ledger.Credits `json:"price,omitempty"`
	Answer string         `json:"answer,omitempty"`
	// Ticks is how many more ticks a "stay" buys. The platform charges for
	// all of them up front and holds the agent where it stands; it is the
	// only action here that costs money to ask for rather than to win.
	Ticks int `json:"ticks,omitempty"`
	// Item is what a "buy" buys, by the name the catalogue showed. Charged in
	// full and burned like a stay; refused, not owed, when the wallet is
	// short, when it is not for sale, or when the agent already has one.
	// A "stock" names the one item its stall sells, at Price, and a buy of
	// a stall's ware transfers the price to its seller instead of burning
	// it — the first purchase at the fair with anyone on the other side.
	Item string `json:"item,omitempty"`
	// Seller is which stall a "buy" of a ware means, when two stock the
	// same name; empty takes the first in roster order.
	Seller string `json:"seller,omitempty"`
	// Text is what a "memo" writes for the agent's next step to read. Empty
	// clears the memo — forgetting is a thing an agent may want to do, and it
	// falls out of the same action rather than needing its own. The last memo
	// in a step wins, mirroring ParseActions' rule for the sentinel itself.
	// Over MaxMemoBytes the write is refused and the previous memo stands: the
	// agent was told the limit, so exceeding it is its own error, and silently
	// keeping the first 512 bytes would hand it a truncated thought it had no
	// way to know was truncated.
	Text string `json:"text,omitempty"`
}

// ParseActions extracts the action list from a step's stdout. The last
// sentinel line wins, so an agent that prints the sentinel in its own logging
// only hurts itself if nothing valid follows. Malformed or missing output is
// an agent fault — the caller treats it as an attempt with no submission.
func ParseActions(stdout string) ([]Action, error) {
	var payload string
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), ActionsSentinel); ok {
			payload, found = rest, true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: no %q line", ErrBadStepOutput, ActionsSentinel)
	}
	var env struct {
		Actions []Action `json:"actions"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadStepOutput, err)
	}
	return env.Actions, nil
}
