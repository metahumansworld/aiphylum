# The Go toolchain lives in ~/.local/go on this machine; a fresh shell has no
# PATH entry for it, so name the binary outright rather than hoping.
GO ?= $(HOME)/.local/go/bin/go
ifeq ($(wildcard $(GO)),)
GO := go
endif

.PHONY: demo demo-imported sim-demo town-demo test vet build clean

## demo: the whole loop in one command — a seeded multi-round episode with
## reference agents on the stub model, ending in the efficiency ladder.
demo:
	$(GO) run ./cmd/dungeond -demo -seed 1 -rounds 8 -trace demo-trace.jsonl

## demo-imported: the same episode with the imported suites in generators/suites
## added to the supply. Imported work IS ranked — a public answer key is still an
## answer key — so it reaches the ladder, and the agents who did it carry an
## asterisk on the board next to the suite's licence and contamination note.
## Its own trace, so the plain demo's stays the pinned one.
demo-imported:
	$(GO) run ./cmd/dungeond -demo -imported -seed 1 -rounds 8 -trace imported-trace.jsonl

## sim-demo: the same cast and the same money on a clock instead of in rounds.
## Unranked by design, so it ends in a chronicle rather than a ladder. Watch it
## live in another shell:
##   go run ./cmd/dungeonctl serve -follow sim-trace.jsonl
sim-demo:
	$(GO) run ./cmd/dungeond -sim -seed 1 -deck 14 -post 1200ms -window 2000ms -latency 250ms -trace sim-trace.jsonl

## town-demo: the living-world track, milestone one — four residents on daily
## schedules walking a small map, no economy and no model. One simulated day in
## under a minute of wall clock. Watch the map live in another shell:
##   go run ./cmd/dungeonctl serve -follow town-trace.jsonl 127.0.0.1:8142
town-demo:
	$(GO) run ./cmd/dungeond -town -days 1 -tick 700ms -trace town-trace.jsonl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

clean:
	rm -f demo-trace.jsonl sim-trace.jsonl imported-trace.jsonl town-trace.jsonl
