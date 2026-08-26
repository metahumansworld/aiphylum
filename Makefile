# The Go toolchain lives in ~/.local/go on this machine; a fresh shell has no
# PATH entry for it, so name the binary outright rather than hoping.
GO ?= $(HOME)/.local/go/bin/go
ifeq ($(wildcard $(GO)),)
GO := go
endif

.PHONY: demo test vet build clean

## demo: the whole loop in one command — a seeded multi-round episode with
## reference agents on the stub model, ending in the efficiency ladder.
demo:
	$(GO) run ./cmd/dungeond -demo -seed 1 -rounds 8 -trace demo-trace.jsonl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

clean:
	rm -f demo-trace.jsonl
