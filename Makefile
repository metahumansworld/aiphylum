# The Go toolchain lives in ~/.local/go on this machine; a fresh shell has no
# PATH entry for it, so name the binary outright rather than hoping.
GO ?= $(HOME)/.local/go/bin/go
ifeq ($(wildcard $(GO)),)
GO := go
endif

.PHONY: demo demo-imported sim-demo town-demo town-mind fair fair-guest fair-vigil fair-scribe fair-haggle fair-rivals fair-lots fair-hucksters fair-hucksters-lot fair-lone-reader fair-rivals-3day fair-hucksters-3day fair-lone-reader-3day fair-lone-reader-swapped fair-lone-reader-swapped-3day fair-lone-reader-sealed-3day fair-lone-reader-sealed-swapped-3day fair-rivals-7day fair-hucksters-7day fair-peddlers-7day fair-peddlers-sealed-7day fair-costermongers-7day fair-costermongers-sealed-7day fair-higgler-7day fair-higgler-sealed-7day fair-higgler-swapped-7day fair-badger-7day fair-badger-swapped-7day fair-badgers-7day fair-lodger-7day fair-diarist-7day fair-diarist-unsold-7day fair-stallholder-7day fair-stallholder-unsold-7day fair-pilgrim-7day fair-newcomer-7day test vet build clean

## demo: the whole loop in one command — a seeded multi-round episode with
## reference agents on the stub model, ending in the efficiency ladder.
demo:
	$(GO) run ./cmd/phylumd -demo -seed 1 -rounds 8 -trace demo-trace.jsonl

## demo-imported: the same episode with the imported suites in generators/suites
## added to the supply. Imported work IS ranked — a public answer key is still an
## answer key — so it reaches the ladder, and the agents who did it carry an
## asterisk on the board next to the suite's licence and contamination note.
## Its own trace, so the plain demo's stays the pinned one.
demo-imported:
	$(GO) run ./cmd/phylumd -demo -imported -seed 1 -rounds 8 -trace imported-trace.jsonl

## sim-demo: the same cast and the same money on a clock instead of in rounds.
## Unranked by design, so it ends in a chronicle rather than a ladder. Watch it
## live in another shell:
##   go run ./cmd/phylumctl serve -follow sim-trace.jsonl
sim-demo:
	$(GO) run ./cmd/phylumd -sim -seed 1 -deck 14 -post 1200ms -window 2000ms -latency 250ms -trace sim-trace.jsonl

## town-demo: the living-world track, milestone one — four residents on daily
## schedules walking a small map, no economy and no model. One simulated day in
## under a minute of wall clock. Watch the map live in another shell:
##   go run ./cmd/phylumctl serve -follow town-trace.jsonl 127.0.0.1:8142
town-demo:
	$(GO) run ./cmd/phylumd -town -days 1 -tick 700ms -trace town-trace.jsonl

## town-mind: the same town, thinking — residents keep memories, talk when they
## meet, and reflect at day's end, all on the offline stub. Its own trace file,
## because town-trace.jsonl is a pinned artefact and a thinking day writes a
## different stream than a silent one.
town-mind:
	$(GO) run ./cmd/phylumd -town -mind -days 1 -tick 700ms -trace town-mind-trace.jsonl

## fair: the composition — the sim's economy at the town's bounty office. The
## cast gets bodies and schedules; bounties post on the hour; only whoever is
## standing at the office sees the board. Deterministic, unranked, zero spend.
## Watch the map live in another shell:
##   go run ./cmd/phylumctl serve -follow fair-trace.jsonl 127.0.0.1:8143
fair:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -trace fair-trace.jsonl

## fair-guest: the fair with a guest — a user-authored agent (the worked
## example in examples/guests/pilgrim.py) takes lodgings and bids against the
## cast on the same money. Its own trace file, because fair-trace.jsonl is a
## pinned artefact and a fair with a stranger in it is a different day.
fair-guest:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/pilgrim.py -trace fair-guest-trace.jsonl

## fair-vigil: the same fair with a guest who pays to stay put.
## examples/guests/vigil.py is examples/guests/pilgrim.py plus exactly one
## behaviour — it buys ticks at the board instead of letting its schedule walk
## it away before the next posting — so the difference between this trace and
## fair-guest-trace.jsonl is the lingering and nothing else.
fair-vigil:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/vigil.py -trace fair-vigil-trace.jsonl

## fair-scribe: the same fair again, with a guest who keeps a memo.
## examples/guests/scribe.py is examples/guests/vigil.py plus exactly one
## behaviour — it writes down what standing at the board has cost it and what
## the work has returned, and stops buying once the first outruns a third of
## the second — so the difference between this trace and fair-vigil-trace.jsonl
## is the remembering and nothing else.
fair-scribe:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/scribe.py -trace fair-scribe-trace.jsonl

## fair-haggle: the same fair again, with a guest who learns what things go for.
## examples/guests/haggler.py is examples/guests/pilgrim.py plus exactly one
## behaviour — it reads observation["results"], the outcome of every auction it
## bid in, and walks its ask up after a win and under the clearing price after a
## loss — so the difference between this trace and fair-guest-trace.jsonl is the
## haggling and nothing else.
fair-haggle:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/haggler.py -trace fair-haggle-trace.jsonl

## fair-rivals: the same fair with two guests, and they are the same guest.
## examples/guests/rival.py imports examples/guests/haggler.py's act verbatim,
## so the two differ in nothing but their names and the order the flags are
## written here — which is the order they are shown the board, and the order a
## tied bid is broken in. haggler learns a price by undercutting whoever beat
## it; this is what that does when the agent it undercuts is doing the same
## thing back.
fair-rivals:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/haggler.py -guest examples/guests/rival.py -trace fair-rivals-trace.jsonl

## fair-lots: the rivals' fair with the queue taken away. Same two copies of
## the same agent, same seed, one change — -tiebreak lot — so a tie at the
## lowest ask is decided by a seeded draw among the tied names instead of by
## who was registered first. The draw is a function of the seed, the bounty
## and a count of the bounty's windows so far — a count the standing winner
## can advance by failing the delivery it just won, which re-rolls the next
## draw but never bends one: the salt is declared on the episode-start line,
## so every draw the day could hold is computable in advance, by anyone.
fair-lots:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -guest examples/guests/haggler.py -guest examples/guests/rival.py -tiebreak lot -trace fair-lots-trace.jsonl

## fair-hucksters: the rivals' fair with the book handed over. Two copies of
## one program again — examples/guests/hawker.py imports huckster.py's act the
## way rival.py imports haggler.py's — but this fair runs -book open, so every
## auction result carries every name and every ask. The huckster has one rule
## where the haggler needed two: price just under the cheapest number in the
## book that is not yours. This is the README's oldest prediction run instead
## of argued, which is why the target exists and why -book open does not
## default. fair-rivals keeps the sealed reference, but these guests are not
## those guests: the strict control is this same command without "-book open",
## which differs from an open day by one flag and, in the trace, one key on
## the start line.
fair-hucksters:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/hawker.py -trace fair-hucksters-trace.jsonl

## fair-hucksters-lot: the open book and the lot at once. If two readers of
## the same book converge on the same number every round — the prediction —
## then the price stops deciding anything and the tie-break decides
## everything, and under arrival order that is the roster deciding. The lot
## replaces the roster with a seeded draw, so this day shows what the open
## book does when the queue is not allowed to launder its results.
fair-hucksters-lot:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/hawker.py -tiebreak lot -trace fair-hucksters-lot-trace.jsonl

## fair-lone-reader: the mixed pair — one open fair, the haggler bidding
## blind on its sealed digests, the huckster reading the book beside it. The
## flags put the haggler first, which under arrival order is the stronger
## seat, and the day is about watching that stop mattering: both open at 20%,
## tie once on the day's first card, and the roster hands it to the haggler —
## the last award the queue ever decides. From its second lesson on the
## huckster prices one undercut under the haggler's floor and wins every
## arith auction it bids in, no tie-break required: strictly-under beats
## first-in-line. This day is one corner of a 2×2 the fair has now run whole
## — fair-rivals is the corner where nobody reads, fair-hucksters the corner
## where both do — and the settled matrix, including which corner is the only
## stable one and why it is the poorest, is in the README's What is
## deliberately not here.
fair-lone-reader:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -book open -guest examples/guests/haggler.py -guest examples/guests/huckster.py -trace fair-lone-reader-trace.jsonl

## fair-rivals-3day: the sealed control at the horizon the ledger settles at.
## The README's last two entries quote a three-day sealed settlement that no
## target ever pinned — the numbers were real, the command was not written
## down. This is that day made repeatable: fair-rivals with -days 3 and its
## own trace file, nothing else changed.
fair-rivals-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -guest examples/guests/haggler.py -guest examples/guests/rival.py -trace fair-rivals-3day-trace.jsonl

## fair-hucksters-3day: the open book at the same horizon — fair-hucksters
## with -days 3 and nothing else. The run the sealed three days are a control
## for, and the first of the two days whose unsolved bounties the README's
## closing entry runs down.
fair-hucksters-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/hawker.py -trace fair-hucksters-3day-trace.jsonl

## fair-lone-reader-3day: the mixed pair at the same horizon — fair-lone-reader
## with -days 3 and nothing else. The second of the two.
fair-lone-reader-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -book open -guest examples/guests/haggler.py -guest examples/guests/huckster.py -trace fair-lone-reader-3day-trace.jsonl

## fair-lone-reader-swapped: the fourth corner of the grid — fair-lone-reader
## with the two -guest flags in the other order, and nothing else changed. The
## README's back-of-the-queue entry quotes this day's 528 and the three-day
## 1,214 from a run whose command was never written down; the corner is the
## one the entry says differs from fair-lone-reader by the opening card alone.
fair-lone-reader-swapped:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 1 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/haggler.py -trace fair-lone-reader-swapped-trace.jsonl

## fair-lone-reader-swapped-3day: the same corner at the ledger's horizon —
## fair-lone-reader-3day with the flags swapped, nothing else.
fair-lone-reader-swapped-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/haggler.py -trace fair-lone-reader-swapped-3day-trace.jsonl

## fair-lone-reader-sealed-3day: the mixed pair with nothing to read, seated
## as fair-lone-reader seats it — the fair-lone-reader-3day command with
## -book open removed. The reader is frozen at its opening here and still
## ties the opening card; which seat keeps that card is the target below.
fair-lone-reader-sealed-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -guest examples/guests/haggler.py -guest examples/guests/huckster.py -trace fair-lone-reader-sealed-3day-trace.jsonl

## fair-lone-reader-sealed-swapped-3day: the sealed control the other way
## round — the frozen reader seated first. The entry's "48 credits in three
## days" is this seating: a reader that never learns still ties the opening
## card, and the queue decides who keeps it.
fair-lone-reader-sealed-swapped-3day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 3 -tick 700ms -guest examples/guests/huckster.py -guest examples/guests/haggler.py -trace fair-lone-reader-sealed-swapped-3day-trace.jsonl

## fair-rivals-7day: the sealed control at seven days — fair-rivals-3day with
## -days 7 and nothing else. Fifty-six cards. The three-day sealed pair ended
## converged and parked at its own floor; this is the same pair given the
## room the open walk is given below, so the two can be read side by side.
fair-rivals-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -guest examples/guests/haggler.py -guest examples/guests/rival.py -trace fair-rivals-7day-trace.jsonl

## fair-hucksters-7day: the open book at seven days — fair-hucksters-3day with
## -days 7 and nothing else. The three-day walk ended still descending, no
## floor in sight. This is the same walk given the days the README's oldest
## prediction needs to either reach the platform reserve or be seen not to.
fair-hucksters-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/huckster.py -guest examples/guests/hawker.py -trace fair-hucksters-7day-trace.jsonl

## fair-peddlers-7day: the open book on the whole board. Two copies of one
## program again — examples/guests/chapman.py imports peddler.py's act the
## way hawker.py imports huckster.py's — and peddler.py is huckster.py with
## its one filter removed: it imports the huckster's pricing rule unchanged
## and bids on every card, brief and oracle included, doing the work each
## card describes. The open week above walked nineteen arith cards to the
## platform's floor while the scholar took 37,114 of the board's 38,833 on
## the judged work no reader touched; this is the same rule sent where the
## money is, for the same seven days, with no ping, no floor of its own and
## one note across every kind. It differs from fair-hucksters-7day in the two
## guest files and nothing else.
fair-peddlers-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/peddler.py -guest examples/guests/chapman.py -trace fair-peddlers-7day-trace.jsonl

## fair-peddlers-sealed-7day: the strict control — the line above without
## "-book open". Same pair, same seed, same seven days; no result ever
## carries a book, so neither peddler learns anything and both bid their
## opening 20% on every card all week. The difference between this trace
## and the open one is the reading and nothing else, which is the same
## control fair-hucksters names and this time pins.
fair-peddlers-sealed-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -guest examples/guests/peddler.py -guest examples/guests/chapman.py -trace fair-peddlers-sealed-7day-trace.jsonl

## fair-costermongers-7day: the peddlers' week with the meter read. The
## costermonger is the peddler with one thing added — after every oracle chain
## it reads its own wallet, writes what the chain cost beside its note, and
## never again asks under the dearest chain of that tier it has paid for. The
## open peddler week was paid to win six tier-one oracles at the reserve; this
## is the same seed, week and seating with a floor read from the meter, and
## what the floor costs the posters is what the trace is for.
fair-costermongers-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/costermonger.py -guest examples/guests/packman.py -trace fair-costermongers-7day-trace.jsonl

## fair-costermongers-sealed-7day: the control — the line above without
## "-book open". The sealed peddlers asked three times the chain's cost on
## every tier-one oracle, so a floor at the cost should never bind here; if
## this trace is the sealed peddler week with two names exchanged, the meter
## changed nothing that the book had not already left alone.
fair-costermongers-sealed-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -guest examples/guests/costermonger.py -guest examples/guests/packman.py -trace fair-costermongers-sealed-7day-trace.jsonl

## fair-higgler-7day: a reader of the rise, seated behind a costermonger for
## seven open days. The higgler is the peddler with one thing added, read from
## the book and not from a wallet: a rival that stood at the reserve on the
## last card of a tier and stands above it now has left the floor, and its ask
## is the higgler's floor for that tier from then on. The costermonger's week
## put an unpaid reader behind a reader whose floor had just risen; this is
## the same week with the back seat reading the rise instead of undercutting
## it, and what that is worth to the back seat and to the posters is what
## the trace is for.
fair-higgler-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/costermonger.py -guest examples/guests/higgler.py -trace fair-higgler-7day-trace.jsonl

## fair-higgler-sealed-7day: the control — the line above without "-book
## open". No result carries a book, so nothing ever rises on one, and a
## higgler with nothing to read is the peddler under another name.
fair-higgler-sealed-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -guest examples/guests/costermonger.py -guest examples/guests/higgler.py -trace fair-higgler-sealed-7day-trace.jsonl

## fair-higgler-swapped-7day: the open week with the flags the other way
## round — the higgler in front, the costermonger behind. The front seat wins
## every tie and runs every chain; whether the seat that reads its meter ever
## gets to rise when it is never paid, and so whether the seat that reads the
## book ever has a rise to read, is the corner this target runs.
fair-higgler-swapped-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/higgler.py -guest examples/guests/costermonger.py -trace fair-higgler-swapped-7day-trace.jsonl

## fair-badger-7day: both rules in one reader, seated behind a costermonger
## for seven open days. The higgler's week left each rule holding one seat —
## the meter needs wins, the book needs rises — so the badger carries both
## and asks at whichever floor stands higher. Behind a costermonger the book
## should fire first and the meter never bind, and the week should land card
## for card on the higgler's; a card that strays is the finding. There is no
## sealed target: a badger with no book and no wins reads nothing, and that
## control already ran under the higgler's name.
fair-badger-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/costermonger.py -guest examples/guests/badger.py -trace fair-badger-7day-trace.jsonl

## fair-badger-swapped-7day: the open week with the seats the other way
## round — the badger in front, the costermonger behind. The front's rise
## leaves the reserve to the back seat, the back wins the next chain, pays
## it, and rises to its own meter; the front reads that rise off the book,
## and its floor becomes the higher of the two meters. The first rise on any
## trace here that carries a rival's cost rather than a reflected price, and
## the card where the front rises before its own meter would is the rule
## working.
fair-badger-swapped-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/badger.py -guest examples/guests/costermonger.py -trace fair-badger-swapped-7day-trace.jsonl

## fair-badgers-7day: the mirror — a badger in either seat, seven open days.
## The question on the table is whether two readers of the book ratchet each
## other's floors up past cost, and the arithmetic says no before the week
## runs: every ask above the walk is a meter reading or a book copy of one,
## so no seat's ask can exceed the dearest chain either seat has paid. Per
## tier, every ask on this trace should sit at or under the highest
## model_call burn so far — checkable line by line, and any ask above it is
## the week disagreeing. Convergence to cost, and a stop there.
fair-badgers-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/badger.py -guest examples/guests/badger.py -trace fair-badgers-7day-trace.jsonl

## fair-lodger-7day: the halves meet — a built agent, the tea steward from
## the builder's own example, takes lodgings at the fair for seven open days
## behind a costermonger. It is seated through the service, not a process:
## each board it is shown is one metered call under the fair's step token,
## the same call the builder page would make for it, and its reply is read by
## the fair's parser exactly as a guest's stdout is. What the week found is
## the stake: a spec carries its whole persona and the protocol into every
## step, the proxy holds the worst case of that before it calls, and the
## worst case of one board is more than the flat 2,000 a guest is staked. So
## the steward is shown the board and refused it, every time — nothing said,
## nothing paid, drift 0. The seam holds; the stake is sized for a script.
fair-lodger-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/costermonger.py -lodger examples/agents/steward.json -trace fair-lodger-7day-trace.jsonl

## fair-diarist-7day: the first thing for sale. examples/guests/diarist.py is
## examples/guests/scribe.py plus exactly one behaviour — it keeps a diary of
## every auction result it is told, as long as its page allows, and buys the
## bigger page when the office has one — and -notebook 400 puts that page up
## for sale: a memo of 4,096 bytes instead of 512, for the rest of the run,
## paid once and burned. The unsold week below is the same seven days with
## nothing on sale, so the difference between the two traces is one purchase
## and the diary it made room for. Conservation closes at drift 0 either way.
fair-diarist-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -notebook 400 -guest examples/guests/diarist.py -trace fair-diarist-7day-trace.jsonl

fair-diarist-unsold-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/diarist.py -trace fair-diarist-unsold-7day-trace.jsonl

## fair-pilgrim-7day: the worked example (examples/guests/pilgrim.py) seated
## at boot for seven days — the control for the week below.
fair-pilgrim-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -guest examples/guests/pilgrim.py -trace fair-pilgrim-7day-trace.jsonl

## fair-newcomer-7day: the same guest, the same seed, but nobody at boot: the
## fair starts with the cast alone and the pilgrim knocks on the door as
## soon as it is open, through phylumctl join, and is seated on the next
## tick. The tick it lands in is wall clock — the trace says which — so this
## is the one week on this list that is not pinned to its seed alone; the
## paired control above is. The knock retries until the door answers, and a
## knock that never lands fails the week rather than running it without the
## pilgrim. Binds -listen (127.0.0.1:8141); a fair or a live daemon already
## on it must be given another.
fair-newcomer-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -trace fair-newcomer-7day-trace.jsonl & \
	n=0; until $(GO) run ./cmd/phylumctl join examples/guests/pilgrim.py; do \
	  n=$$((n+1)); if [ $$n -ge 30 ]; then echo "fair-newcomer-7day: the knock never landed"; kill $$!; wait; exit 1; fi; sleep 1; \
	done; wait

## fair-stallholder-7day: the first thing bought that the town can see.
## examples/guests/stallholder.py is examples/guests/scribe.py plus exactly
## one behaviour — it buys a stall when the office has one and the purse holds
## four times the price — and -stall 400 puts one up for sale: a pitch on the
## town square, assigned by the office, carried on the bought event and drawn
## there by the spectator's page for the rest of the record. It does nothing
## for its owner but stand there. The unsold week below is the same seven days
## with nothing on sale, so the difference between the two traces is one
## purchase and one cell. Conservation closes at drift 0 either way.
fair-stallholder-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -stall 400 -guest examples/guests/stallholder.py -trace fair-stallholder-7day-trace.jsonl

fair-stallholder-unsold-7day:
	$(GO) run ./cmd/phylumd -fair -seed 1 -days 7 -tick 700ms -book open -guest examples/guests/stallholder.py -trace fair-stallholder-unsold-7day-trace.jsonl fair-pilgrim-7day-trace.jsonl fair-newcomer-7day-trace.jsonl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

# Every trace named here can be regenerated from a seed, so removing one costs
# nothing. live-trace.jsonl is deliberately absent and must not be added: it is
# the only record of a run that cannot be run again, which is the same reason
# live mode appends to it and never truncates it.
clean:
	rm -f demo-trace.jsonl sim-trace.jsonl imported-trace.jsonl town-trace.jsonl town-mind-trace.jsonl fair-trace.jsonl fair-guest-trace.jsonl fair-vigil-trace.jsonl fair-scribe-trace.jsonl fair-haggle-trace.jsonl fair-rivals-trace.jsonl fair-lots-trace.jsonl fair-hucksters-trace.jsonl fair-hucksters-lot-trace.jsonl fair-lone-reader-trace.jsonl fair-rivals-3day-trace.jsonl fair-hucksters-3day-trace.jsonl fair-lone-reader-3day-trace.jsonl fair-rivals-7day-trace.jsonl fair-hucksters-7day-trace.jsonl fair-peddlers-7day-trace.jsonl fair-peddlers-sealed-7day-trace.jsonl fair-lone-reader-swapped-trace.jsonl fair-lone-reader-swapped-3day-trace.jsonl fair-lone-reader-sealed-3day-trace.jsonl fair-lone-reader-sealed-swapped-3day-trace.jsonl fair-costermongers-7day-trace.jsonl fair-costermongers-sealed-7day-trace.jsonl fair-higgler-7day-trace.jsonl fair-higgler-sealed-7day-trace.jsonl fair-higgler-swapped-7day-trace.jsonl fair-badger-7day-trace.jsonl fair-badger-swapped-7day-trace.jsonl fair-badgers-7day-trace.jsonl fair-lodger-7day-trace.jsonl fair-diarist-7day-trace.jsonl fair-diarist-unsold-7day-trace.jsonl fair-stallholder-7day-trace.jsonl fair-stallholder-unsold-7day-trace.jsonl

## serve: the service — the builder page and one built agent, from
## examples/agents, on 127.0.0.1:8151. Open http://127.0.0.1:8151/ and sign
## in — offline the link lands in the log — then describe an agent in the chat
## pane, or edit its nodes, and save; every agent the page saves is kept
## (phylum-agents.db) beside the books (phylum-service.db, phylum-accounts.db)
## across restarts. On the stub, zero API calls; set OPENROUTER_API_KEY and
## the same command spends real money behind each user's $1 grant, a draft
## from the same grant as a reply. The same over curl:
##   curl -s localhost:8151/auth/request -d '{"email":"you@example.com"}'
##   curl -s localhost:8151/auth/verify -d '{"token":"<from the log>"}'
##   curl -s localhost:8151/v1/draft -H 'Authorization: Bearer <session>' -d '{"spec":{"version":"phylum-agent/1"},"request":"a steward for a tea shop"}'
##   curl -s localhost:8151/v1/agents -H 'Authorization: Bearer <session>' -d @examples/agents/steward.json
##   curl -s localhost:8151/a/<id>/messages -d '{"text":"any jasmine tea?"}'
##   curl -s localhost:8151/a/<id>/events -d '{"order":"two tins of jasmine"}'   # once the spec has a webhook
## The page's endpoint line carries the widget snippet for any site, and the
## Tools node calls the owner's own https endpoints. On your own machine,
## -serve-insecure-tools lets a tool reach http and 127.0.0.1; never deploy it.
serve:
	$(GO) run ./cmd/phylumd -serve -agent examples/agents/steward.json
