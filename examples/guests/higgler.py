#!/usr/bin/env python3
"""higgler — the peddler, reading a rival's rise.

Run it into town with:

    make fair-higgler-7day            # seated behind a costermonger, seven open days
    make fair-higgler-sealed-7day     # the same seven days without -book open
    make fair-higgler-swapped-7day    # the open week with the seats the other way round

It is examples/guests/peddler.py with one thing added, and the thing is read
from the book rather than from a wallet. The costermonger's week put a reader
that had never been paid behind a reader whose floor had just risen, and the
back seat took the losses the front seat declined. The book had shown it the
rise — on the card it won, the cheapest rival stood at 94 where the floor was
36 — and the huckster's rule read that ask as a ratio of its own: 94 times one
percent over an ask of 36, which is two, undercut to one. The rule reads a
rival against the ask it made, and once the reserve has raised that ask off
the note, the ratio says almost nothing. So a rise on the book was read and
lost, and the reader stayed at the floor the rival had just left.

This file keeps one more thing than the huckster's note: for each tier of
oracle card, which rivals stood at the reserve on the last such card it bid
on. A rival that stood at the floor last time and stands above it now has
left the floor, and a bidder that leaves the floor on work that costs money
has read something — its meter, or its own losses — that this reader has not
paid to learn. Its ask is the floor for that tier from then on, the highest
such ask if there are several, and a floor once set is not lowered. That is
the whole rule: a rival's rise is a floor to match, not a price to undercut.
Nothing here reads a wallet, a price table or a constant; every number it
floors at was somebody else's ask, on a book the platform published.

Two things follow from reading the rise after the auction rather than before
it, and both are kept. The card a rival leaves the floor on is already
decided when the rise is read, so the first such card is the reader's at the
floor, whatever it costs. And a rival that never leaves the floor teaches
nothing: if the seat in front never rises, this file is the peddler under
another name, and the sealed control, where no book arrives at all, is that
by construction.

The chain and the brief are the peddler's, imported; the pricing rule is the
huckster's, imported through the peddler; the memo is this file's, because
the huckster's read_note keeps its three keys and drops the rest. Results
carry a bounty's id and its book and nothing about the card, so this file
notes the tier and reserve of every oracle card it asks on, keyed by id, to
place the result when it comes. The huckster's rule is not changed: reading
the ratio against the maximum instead of the ask would fix the arithmetic and
rewrite every open-book trace on the page.
"""

import json

import aiphylum
from huckster import parse, read_book, read_note, solve
from peddler import brief, consult

MAX_MEMO_BYTES = aiphylum.MAX_MEMO_BYTES


def read_rises(observation):
    """This file's own keys, as the last memo left them: bids, the tier and
    reserve of every oracle card asked on and not yet resolved, by id; at,
    the rivals that stood at the reserve on the last card of each tier;
    floor, the highest rise read on each tier. Everything defaulted."""
    raw = observation.get("memo")
    note = {}
    if raw:
        try:
            note = json.loads(raw)
        except ValueError:
            note = {}
    if not isinstance(note, dict):
        note = {}
    bids = note.get("bids") if isinstance(note.get("bids"), dict) else {}
    at = note.get("at") if isinstance(note.get("at"), dict) else {}
    floor = note.get("floor") if isinstance(note.get("floor"), dict) else {}
    out = {"bids": {}, "at": {}, "floor": {}}
    for bounty, placed in bids.items():
        try:
            tier, reserve = placed
            out["bids"][str(bounty)] = [str(int(tier)), int(reserve)]
        except (TypeError, ValueError):
            pass
    for tier, names in at.items():
        if isinstance(names, list):
            out["at"][str(tier)] = [str(n) for n in names]
    for tier, credits in floor.items():
        try:
            out["floor"][str(tier)] = max(0, int(credits))
        except (TypeError, ValueError):
            pass
    return out


def read_rival_rises(rises, me, results):
    """The one rule. For every result on an oracle card this file asked on:
    a rival that stood at the reserve on the last card of this tier and
    stands above it now has left the floor, and its ask becomes the tier's
    floor if it is higher than the floor already read. Then note who stands
    at the reserve now, for the next card of the tier."""
    for r in results:
        placed = rises["bids"].pop(str(r.get("bounty")), None)
        if not placed or not r.get("book"):
            continue  # not an oracle card this file asked on, or a sealed result
        tier, reserve = placed
        rivals = {}
        for e in r["book"]:
            if e.get("agent") != me and int(e.get("asked") or 0) > 0:
                rivals[str(e.get("agent"))] = int(e.get("asked"))
        risen = [ask for name, ask in rivals.items()
                 if name in rises["at"].get(tier, []) and ask > reserve]
        if risen:
            rises["floor"][tier] = max(rises["floor"].get(tier, 0), max(risen))
        rises["at"][tier] = sorted(name for name, ask in rivals.items() if ask == reserve)


def memo(note, rises):
    """The huckster's note with this file's keys beside it, kept under the
    platform's cap by forgetting the oldest unresolved bids first."""
    bids = dict(rises["bids"])
    while True:
        text = json.dumps(dict(note, bids=bids, at=rises["at"], floor=rises["floor"]),
                          separators=(",", ":"))
        if len(text.encode()) <= MAX_MEMO_BYTES or not bids:
            return {"type": "memo", "text": text}
        del bids[next(iter(bids))]


def act(observation, wallet):
    note = read_note(observation)
    rises = read_rises(observation)

    if observation["phase"] != "bid":
        task = observation["task"]
        kind, spec = parse(task["prompt"])
        if kind == "arith":
            answer = solve(spec["expr"])
        elif kind == "brief":
            answer = brief(spec)
        elif kind == "oracle":
            answer = consult(spec)
        else:
            return [memo(note, rises)]  # a card this file does not know how to work
        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo(note, rises),
        ]

    results = observation.get("results") or []
    read_book(note, wallet.id, results)
    read_rival_rises(rises, wallet.id, results)

    actions = []
    for b in observation.get("bounties") or []:
        price = max(b["reserve"], b["max_payout"] * note["pct"] // 100)
        kind, _ = parse(b["prompt"])
        if kind == "oracle":
            tier = str(b["tier"])
            price = max(price, rises["floor"].get(tier, 0))
            rises["bids"][b["id"]] = [tier, int(b["reserve"])]
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    actions.append(memo(note, rises))
    return actions


if __name__ == "__main__":
    aiphylum.run(act)
