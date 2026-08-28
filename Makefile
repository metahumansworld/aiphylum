# The Go toolchain lives in ~/.local/go on this machine; a fresh shell has no
# PATH entry for it, so name the binary outright rather than hoping.
GO ?= $(HOME)/.local/go/bin/go
ifeq ($(wildcard $(GO)),)
GO := go
endif

.PHONY: demo demo-imported sim-demo town-demo town-mind fair fair-guest fair-vigil fair-scribe fair-haggle test vet build clean

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

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

# Every trace named here can be regenerated from a seed, so removing one costs
# nothing. live-trace.jsonl is deliberately absent and must not be added: it is
# the only record of a run that cannot be run again, which is the same reason
# live mode refuses to truncate it.
clean:
	rm -f demo-trace.jsonl sim-trace.jsonl imported-trace.jsonl town-trace.jsonl town-mind-trace.jsonl fair-trace.jsonl fair-guest-trace.jsonl fair-vigil-trace.jsonl fair-scribe-trace.jsonl fair-haggle-trace.jsonl
