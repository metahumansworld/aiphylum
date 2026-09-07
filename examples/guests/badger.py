#!/usr/bin/env python3
"""badger — the peddler, reading the meter and the rise both.

Run it into town with:

    make fair-badger-7day          # seated behind a costermonger, seven open days
    make fair-badger-swapped-7day  # the open week with the seats the other way round
    make fair-badgers-7day         # a badger in either seat: the mirror week

It is examples/guests/peddler.py with both things added, and nothing new of
its own. The costermonger reads its meter: after an oracle chain has run, the
wallet's drop is the least the same tier is asked for again. The higgler
reads the rise: a rival that stood at the reserve and stands above it now has
left the floor, and its ask is the floor to match. The higgler's week showed
each rule holds exactly one seat — the meter needs wins to read and the front
seat takes every tie, the book needs rises to read and only a paid rival ever
rises — so a seat-independent reader has to carry both. This file is that
reader: both floors under the huckster's walk, and an oracle is asked at
whichever stands higher. The rules are imported, not rewritten — read_costs
from the costermonger, read_rises and read_rival_rises from the higgler — and
the higgler's rule already reads no ask of this file's own, so a badger
facing a badger reads its rival and not itself.

What the three weeks should show is written down here before any of them has
run, because the arithmetic is nearly forced, and a week that strays from its
line is the finding. Behind a costermonger, the book fires first and the
meter never binds: the one blind win burns 49 while the book reads the
rival's 94, so the week should land card for card on the higgler's —
thirteen down, the floor matched from the fourth tier-one card. In front of
one, the meter fires first and then hands the book something new to do: the
front's rise leaves the reserve to the back seat, the back seat wins the
next chain, pays it, and rises to its own meter — and that rise, read off
the book, is the first on any trace here that carries a rival's cost rather
than the reader's own price reflected back. The front's floor becomes the
higher of the two meters, so the week walks the costermonger-and-packman
week card for card only until the back's blind chain costs more than the
front's dearest, and the card where the front rises a step early is the rule
working. And in the mirror week nothing ratchets, for a reason tighter than
who wins ties: every ask above the walk is a meter reading or a book copy of
one, so no seat's ask can exceed the dearest chain either seat has paid —
per tier, every ask on the trace at or under the highest model_call burn so
far, which anyone can check line by line. The open book converges the pair
to cost and stops there.

One corner is this file's own, and it is kept rather than fixed. The ask is
the higher floor, so a badger in the back seat declines any card priced
between its own meter and the rival's ask — its one paid chain said 49, the
book says 94, and it stands at 94. Whether that is money left on the table or
proper distrust of a meter that has read one cheap chain is what the traces
are for; a reader that wanted to know would have to price the chain before
running it, and the only place that number exists is in the platform's
source. There is no sealed target: a badger with no book and no wins reads
nothing, and that control already ran under the higgler's name.
"""

import json

import aiphylum
from costermonger import read_costs
from higgler import read_rises, read_rival_rises
from huckster import parse, read_book, read_note, solve
from peddler import brief, consult

MAX_MEMO_BYTES = aiphylum.MAX_MEMO_BYTES


def memo(note, costs, rises):
    """Both readings beside the huckster's note, kept under the platform's
    cap by forgetting the oldest unresolved bids first."""
    bids = dict(rises["bids"])
    while True:
        text = json.dumps(
            dict(note, cost=costs, bids=bids, at=rises["at"], floor=rises["floor"]),
            separators=(",", ":"))
        if len(text.encode()) <= MAX_MEMO_BYTES or not bids:
            return {"type": "memo", "text": text}
        del bids[next(iter(bids))]


def act(observation, wallet):
    note = read_note(observation)
    costs = read_costs(observation)
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
            # The costermonger's meter, read after the work: the attempt
            # purse opened this step at wallet.balance, and the chain is the
            # only thing that has drawn on it since.
            spent = wallet.balance - aiphylum.Model().wallet().balance
            tier = str(task["tier"])
            costs[tier] = max(costs.get(tier, 0), spent)
        else:
            return [memo(note, costs, rises)]  # a card this file does not know how to work
        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo(note, costs, rises),
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
            price = max(price, costs.get(tier, 0), rises["floor"].get(tier, 0))
            rises["bids"][b["id"]] = [tier, int(b["reserve"])]
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    actions.append(memo(note, costs, rises))
    return actions


if __name__ == "__main__":
    aiphylum.run(act)
