#!/usr/bin/env python3
"""costermonger — the peddler, reading the meter.

Run it into town with:

    make fair-costermongers-7day          # seven open days, the run this file is for
    make fair-costermongers-sealed-7day   # the same seven days without -book open

It is examples/guests/peddler.py with one thing added. The peddler bids on
every card with the huckster's rule and no floor of its own beyond the card's
reserve, and the open week found where that leads: on every tier-one oracle
the chain costs more than the reserve, so once the walk reached the floor the
peddler was paid to win six of them — delivered, checked, paid its ask, and
poorer for it. Its docstring names a floor that knew the chain's price as the
second rule the file exists not to have, and refuses it. This file is that
floor, on its own, so the refusal and the floor can be read side by side.

The floor is read, not computed. After an oracle chain has run, this file
reads its wallet the way anyone reads a meter — the balance the step began
with, less the balance the chain left behind — and writes what the chain cost
beside the huckster's note, under the card's tier. At the next bid step an
oracle is never asked under the dearest chain of its tier this agent has paid
for. That is the whole rule: what the meter said last time is the least the
same kind of work is asked for next time. Nothing here divides a maximum by a
constant, reads a price table, or estimates a token; the number it floors at
was debited from its own wallet, and the trace's model_call lines are where
anyone can check it.

Two things follow from reading the meter after the work instead of the
platform's arithmetic before it, and both are kept rather than fixed. The
floor lags: it is the dearest chain so far, and a chain dearer than any yet
paid for is still a delivery paid for, once, before the floor rises to it. And
the first chain of every tier is bought blind, because there is nothing to
read until something has been read. A reader that wanted neither would have
to know the cost before the call, and the only place that number exists
before the call is in the platform's source.

The chain and the brief are the peddler's own, imported; the pricing rule is
the huckster's, imported through the peddler; the memo is this file's,
because the huckster's read_note keeps its three keys and drops the rest, and
the cost has to travel beside them. One note still: the walk is the
huckster's, on every kind, and the floor is a floor under it, not a second
walk.
"""

import json

import aiphylum
from huckster import parse, read_book, read_note, solve
from peddler import brief, consult


def read_costs(observation):
    """The dearest chain paid for, per tier, as the last memo left it: a dict
    of tier -> credits, every entry defaulted to nothing known."""
    raw = observation.get("memo")
    note = {}
    if raw:
        try:
            note = json.loads(raw)
        except ValueError:
            note = {}
    costs = note.get("cost") if isinstance(note, dict) else None
    out = {}
    if isinstance(costs, dict):
        for tier, credits in costs.items():
            try:
                out[str(int(tier))] = max(0, int(credits))
            except (TypeError, ValueError):
                pass
    return out


def memo(note, costs):
    """The huckster's note with the meter's readings beside it."""
    text = json.dumps(dict(note, cost=costs), separators=(",", ":"))
    return {"type": "memo", "text": text}


def act(observation, wallet):
    note = read_note(observation)
    costs = read_costs(observation)

    if observation["phase"] != "bid":
        task = observation["task"]
        kind, spec = parse(task["prompt"])
        if kind == "arith":
            answer = solve(spec["expr"])
        elif kind == "brief":
            answer = brief(spec)
        elif kind == "oracle":
            answer = consult(spec)
            # The meter, read after the work: the attempt purse opened this
            # step at wallet.balance, and the chain is the only thing that
            # has drawn on it since.
            spent = wallet.balance - aiphylum.Model().wallet().balance
            tier = str(task["tier"])
            costs[tier] = max(costs.get(tier, 0), spent)
        else:
            return [memo(note, costs)]  # a card this file does not know how to work
        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo(note, costs),
        ]

    read_book(note, wallet.id, observation.get("results") or [])

    actions = []
    for b in observation.get("bounties") or []:
        price = max(b["reserve"], b["max_payout"] * note["pct"] // 100)
        kind, _ = parse(b["prompt"])
        if kind == "oracle":
            # The floor: never under the dearest chain of this tier paid for.
            price = max(price, costs.get(str(b["tier"]), 0))
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    actions.append(memo(note, costs))
    return actions


if __name__ == "__main__":
    aiphylum.run(act)
