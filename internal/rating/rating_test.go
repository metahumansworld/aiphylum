package rating

import (
	"testing"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/bounty"
	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
)

var t0 = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// Plan verification #8: the ladder resists sandbagging. Two synthesised
// agents — one taking only cheap certainties, one attempting hard bounties
// with occasional failures — and the difficulty-weighted metric must rank the
// second higher. Without this the leaderboard is unsound.
func TestLadderResistsSandbagging(t *testing.T) {
	l := New(Config{MinAttempts: 5, MinTiers: 2})

	// Both agents solve tasks with the same reference cost; only tier and
	// reliability differ. Payouts come from the real schedule so the test
	// breaks if the schedule stops being superlinear.
	ref := ledger.Credits(1000)

	// Sandbagger: ten tier-1 certainties, all solved, minimal burn. Attempts
	// a couple of tier-2s (also easy) so the tier gate alone doesn't decide
	// the test — the ratio has to.
	for i := 0; i < 8; i++ {
		l.Record(Attempt{
			Agent: "sandbagger", Tier: 1, Success: true,
			Earned: bounty.PayoutFor(ref, 1), Burned: 1200,
			Time: t0,
		})
	}
	for i := 0; i < 2; i++ {
		l.Record(Attempt{
			Agent: "sandbagger", Tier: 2, Success: true,
			Earned: bounty.PayoutFor(ref, 2), Burned: 2400,
			Time: t0,
		})
	}

	// Climber: hard bounties, tier 4-5, real burn, and one failure in three —
	// a failure that costs full burn and earns nothing.
	for i := 0; i < 6; i++ {
		tier := 4 + i%2
		ok := i%3 != 2 // two failures out of six
		var earned ledger.Credits
		if ok {
			earned = bounty.PayoutFor(ref, tier)
		}
		l.Record(Attempt{
			Agent: "climber", Tier: tier, Success: ok,
			Earned: earned, Burned: ledger.Credits(1200 * tier),
			Time: t0,
		})
	}

	rows := l.Board(t0)
	if len(rows) != 2 {
		t.Fatalf("board has %d rows, want 2", len(rows))
	}
	if !rows[0].Ranked || !rows[1].Ranked {
		t.Fatalf("both agents should clear the gates: %+v", rows)
	}
	if rows[0].Agent != "climber" {
		t.Fatalf("board leader = %s (eff %.2f) over %s (eff %.2f); want climber on top",
			rows[0].Agent, rows[0].Efficiency, rows[1].Agent, rows[1].Efficiency)
	}
}

// Failures land in the denominator: an agent with the same earnings but
// failed attempts on top ranks strictly lower.
func TestFailuresCountInDenominator(t *testing.T) {
	l := New(Config{MinAttempts: 2, MinTiers: 1})

	for _, agent := range []string{"clean", "wasteful"} {
		for i := 0; i < 3; i++ {
			l.Record(Attempt{Agent: agent, Tier: 2, Success: true, Earned: 6000, Burned: 1000, Time: t0})
		}
	}
	// Same earnings, plus two expensive failures.
	for i := 0; i < 2; i++ {
		l.Record(Attempt{Agent: "wasteful", Tier: 2, Success: false, Earned: 0, Burned: 2000, Time: t0})
	}

	rows := l.Board(t0)
	if rows[0].Agent != "clean" {
		t.Fatalf("leader = %s, want clean", rows[0].Agent)
	}
	var wasteful Row
	for _, r := range rows {
		if r.Agent == "wasteful" {
			wasteful = r
		}
	}
	if wasteful.Burned != 3000+4000 {
		t.Fatalf("wasteful burned = %d, want 7000 (failures included)", wasteful.Burned)
	}
}

// The volume gates: too few attempts, or attempts all in one tier, and the
// agent appears unranked.
func TestVolumeGates(t *testing.T) {
	l := New(Config{MinAttempts: 5, MinTiers: 2})

	// Four attempts across two tiers: fails the attempt gate.
	for i := 0; i < 4; i++ {
		l.Record(Attempt{Agent: "thin", Tier: 1 + i%2, Success: true, Earned: 100, Burned: 10, Time: t0})
	}
	// Six attempts all tier 1: fails the tier gate — this is the cheap-
	// certainty camper the gate exists for.
	for i := 0; i < 6; i++ {
		l.Record(Attempt{Agent: "camper", Tier: 1, Success: true, Earned: 100, Burned: 10, Time: t0})
	}
	// Five attempts, two tiers: ranked.
	for i := 0; i < 5; i++ {
		l.Record(Attempt{Agent: "legit", Tier: 1 + i%2, Success: true, Earned: 100, Burned: 10, Time: t0})
	}

	rows := l.Board(t0)
	byAgent := map[string]Row{}
	for _, r := range rows {
		byAgent[r.Agent] = r
	}
	if byAgent["thin"].Ranked {
		t.Fatal("thin cleared the attempt gate with 4 attempts")
	}
	if byAgent["camper"].Ranked {
		t.Fatal("camper cleared the tier gate with one tier")
	}
	if !byAgent["legit"].Ranked {
		t.Fatal("legit did not rank")
	}
	if rows[0].Agent != "legit" {
		t.Fatalf("ranked rows must sort above unranked; top = %s", rows[0].Agent)
	}
}

// The rolling window: old attempts age out of the standings.
func TestRollingWindow(t *testing.T) {
	l := New(Config{Window: time.Hour, MinAttempts: 1, MinTiers: 1})

	l.Record(Attempt{Agent: "a", Tier: 1, Success: true, Earned: 1000, Burned: 100, Time: t0.Add(-2 * time.Hour)})
	l.Record(Attempt{Agent: "a", Tier: 1, Success: true, Earned: 500, Burned: 100, Time: t0.Add(-10 * time.Minute)})

	rows := l.Board(t0)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Attempts != 1 || rows[0].Earned != 500 {
		t.Fatalf("window leaked old attempts: %+v", rows[0])
	}
}

// Zero burn cannot rank (division by zero guarded, and a no-spend agent has
// proven nothing).
func TestZeroBurnUnranked(t *testing.T) {
	l := New(Config{MinAttempts: 1, MinTiers: 1})
	l.Record(Attempt{Agent: "ghost", Tier: 1, Success: true, Earned: 1000, Burned: 0, Time: t0})
	rows := l.Board(t0)
	if rows[0].Ranked {
		t.Fatal("zero-burn agent ranked")
	}
}
