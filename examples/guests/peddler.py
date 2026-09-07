#!/usr/bin/env python3
"""peddler — the huckster, let loose on the whole board.

Run it into town with:

    make fair-peddlers-7day          # seven open days, the run this file is for
    make fair-peddlers-sealed-7day   # the same seven days without -book open

It is examples/guests/huckster.py with one line removed. The huckster bids on
arithmetic and nothing else — "a stranger doesn't pay for oracles it can't
vouch for" — and so the open book's whole walk, three README entries of it,
happened on nineteen of a week's fifty-six cards, the cheapest nineteen: a
seven-day open board paid out 38,833 and 37,114 of that went to the scholar
for brief and oracle work no reader ever bid on. This file bids on every
card. The pricing rule is not copied; it is imported, so whatever the peddler
does on a brief or an oracle it does with the huckster's own read_book, the
same undercut, the same probe, the same ceiling and the same missing floor.
The only code of its own is the work: an arithmetic card is computed, a brief
is the standard's line assembled from the spec, an oracle is the chain the
card describes, run at the card's own model and max_tokens through the same
metered proxy the scholar uses.

Three things are decided here, and each is decided the same way: the way that
adds no second rule.

One note for every kind. The memo's percentage is a fraction of the card's
maximum, and the whole point of a ratio is that it travels — from a 240 card
to a 2,435 one, and from an arith to an oracle just the same. A note per kind
would be three walks under one name. This is one walk taking eight lessons a
day instead of three, and wherever it ends up, every kind is priced there.

No ping. The huckster made one minimal model call on every arith card, to
feel the meter, and it cost 22 credits a card — more than the 12 a tier-one
card pays at the reserve, which the pinned seven-day trace already shows and
no entry said. The peddler's only model calls are the ones the card requires:
none on arithmetic, none on a brief, the chain on an oracle. Every credit it
burns is the price of an answer, so the trace's burned-against-payout is
measured on the work alone.

No floor of its own. The huckster's floor is each card's reserve, the
platform's number, and the peddler keeps exactly that. An oracle chain costs
what it costs — a tier-one chain around 58 to 94 credits on cards whose
reserves are 36 to 70 — and a reader that undercuts every rival it sees has
nothing in its rule that knows what the answer will cost to produce. Where
the ask falls under the cost, the peddler pays to win, and the trace names
the card. A floor that knew the chain's price would be the second rule this
file exists not to have; the sealed control is the same pair with nothing to
read, and the difference between the two weeks is the book.
"""

import json

import soscitea
from huckster import parse, read_book, read_note, solve


def consult(spec):
    """The oracle chain as the card states it: spec["calls"] calls at
    spec["model"] and spec["max_tokens"], each fed the last reply, starting
    from spec["message"]. The stub's answer is a hash of the exact request
    body, so nothing is added to the call and nothing is left out."""
    model = soscitea.Model()
    content = spec["message"]
    for _ in range(spec["calls"]):
        reply = model.complete(
            model=spec["model"],
            messages=[{"role": "user", "content": content}],
            max_tokens=spec["max_tokens"],
        )
        content = reply.text
    return content


def brief(spec):
    """The brief's standard, assembled from its own spec: every field in the
    spec's order, "field: value", joined with "; ". The rubric is the line
    the card asks for, so reading carefully passes a grader never seen."""
    return "; ".join("%s: %s" % (f, spec["facts"][f]) for f in spec["fields"])


def act(observation, wallet):
    note = read_note(observation)

    if observation["phase"] != "bid":
        # The attempt phase, which prices nothing; the wallet is the attempt
        # purse. The note passes through untouched, as the huckster's does.
        memo = {"type": "memo", "text": json.dumps(note, separators=(",", ":"))}
        task = observation["task"]
        kind, spec = parse(task["prompt"])
        if kind == "arith":
            answer = solve(spec["expr"])
        elif kind == "brief":
            answer = brief(spec)
        elif kind == "oracle":
            answer = consult(spec)
        else:
            return [memo]  # a card this file does not know how to work
        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo,
        ]

    # The huckster's bid step, less its one filter. The books arrive exactly
    # once, here; the wallet's id is this agent's name, the line of the book
    # to look away from.
    read_book(note, wallet.id, observation.get("results") or [])

    actions = []
    for b in observation.get("bounties") or []:
        price = max(b["reserve"], b["max_payout"] * note["pct"] // 100)
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    actions.append({"type": "memo", "text": json.dumps(note, separators=(",", ":"))})
    return actions


if __name__ == "__main__":
    soscitea.run(act)
