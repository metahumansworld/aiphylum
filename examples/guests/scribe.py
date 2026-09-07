#!/usr/bin/env python3
"""scribe — a guest who keeps a note and stops paying for a habit that isn't
paying back.

Run it into town with:

    make fair-scribe

It is examples/guests/vigil.py with one thing added, again on purpose: the
same arithmetic-only nerve, the same twenty-percent price, the same purchase
of standing still at the board. The difference between the two days is one
behaviour, and nothing else.

That behaviour is remembering. vigil buys ticks at the board every time it can
afford to, all day, and cannot do otherwise — a step is a fresh process, so
every step it has ever run is its first. It has no way to notice that the
standing is costing it more than the standing is bringing in, because noticing
requires two moments and it only ever has one.

The platform hands over the missing moment. Whatever text an agent writes in a
{"type": "memo"} action comes back as observation["memo"] on its next step,
and the one after, across attempts and across days, until it writes something
else. The platform stores it and does not read it: nothing in here changes a
price or a payout. It is not private, though — accepted memos go into the
trace like everything else, and the trace is the published artefact. So this
file's running tally is on the record, which seems only right for a scribe.

What the note holds is a small ledger of one trade:

    p  credits paid for standing, cumulative
    w  credits the work has actually returned, net, cumulative
    b  the balance at the last step, so the next one can take a difference
    c  what the last step asked to spend on standing, which had not been
       charged yet when that balance was read
    a  attempts made — written from the attempt phase, read in the bid phase,
       which is the one number here that no observation could ever supply

`w` is worth a sentence, because it is inferred rather than told. Between two
bid steps the only things that move this wallet are the standing we bought,
the tokens an attempt burned, and a payout if the attempt was right. We know
what we spent standing, so the rest of the difference is the work — earnings
net of what earning them cost. Nobody reports that number to us. It falls out
of two balances and a memory of what happened in between.
"""

import json
import re

import soscitea

# How many ticks to buy at a time — vigil's number, unchanged, so the two days
# differ by when the buying stops and not by how much each purchase is worth.
WAIT_TICKS = 4

# The discipline: the standing bill stays under a third of what the work has
# actually returned. Above that the habit is eating the wages that pay for it.
# Any fraction is a judgement rather than a fact; this one is stated out loud
# so it can be argued with, which is more than a number buried in a condition
# can manage.
EARNINGS_SHARE = 3


def parse(prompt):
    """Every board card leads with a "[kind]" tag and ends with a
    "spec: {...}" line. Returns (kind, spec dict); unknown cards ("", {})."""
    m = re.match(r"\[(\w+)\]", prompt)
    kind = m.group(1) if m else ""
    spec = {}
    for line in prompt.splitlines():
        if line.startswith("spec: "):
            try:
                spec = json.loads(line[len("spec: "):])
            except ValueError:
                pass
    return kind, spec


def solve(expr):
    """Normal-precedence arithmetic, and a refusal to eval anything that
    isn't: digits, + - *, spaces, nothing else reaches the interpreter."""
    if not re.fullmatch(r"[0-9+\-* ]+", expr):
        raise ValueError("not arithmetic: %r" % expr)
    return eval(expr)  # noqa: S307


def read_note(observation):
    """The memo, decoded, with every field defaulted.

    A memo comes back exactly as it was written, which means a memo written by
    an older version of this file — or by nothing at all, on the first step of
    the first day — is a case that has to be handled rather than assumed away.
    Anything unreadable is treated as no memory at all: a fresh start is wrong
    but survivable, where a crash on the first step is neither.
    """
    raw = observation.get("memo")
    note = {}
    if raw:
        try:
            note = json.loads(raw)
        except ValueError:
            note = {}
    if not isinstance(note, dict):
        note = {}
    return {k: int(note.get(k) or 0) for k in ("p", "w", "b", "c", "a")}


def keep_vigil(observation, wallet, note):
    """Whether to buy another stretch at the board.

    vigil's three conditions, unchanged and still all necessary — there has to
    be a price at all, we must not already hold standing we have not used, and
    a full stretch has to cost under a twentieth of the wallet. On top of them,
    one condition vigil could not have expressed:

        while the work has returned nothing yet, keep buying;
        after that, only while the standing bill is under a third of it.

    The first half matters as much as the second. An agent that demands proof
    before its first purchase never makes one, and so never gets the proof —
    the only way to find out what presence is worth is to buy some. This is
    where the memory earns its keep in both directions: it is what lets the
    experiment stop, and `w == 0` is what lets it start.
    """
    price = observation.get("stay_price") or 0
    if not price or not observation.get("place"):
        return None
    if observation.get("stay_ticks_left"):
        return None  # already paid for; asking again just buys the minute twice
    if wallet.balance < price * WAIT_TICKS * 20:
        return None
    if note["w"] and note["p"] * EARNINGS_SHARE >= note["w"]:
        return None  # the habit has outrun the wages
    return {"type": "stay", "ticks": WAIT_TICKS}


def act(observation, wallet):
    note = read_note(observation)

    if observation["phase"] != "bid":
        # The attempt phase. The bid phase can see the board and the balance;
        # what it cannot see is that this agent was ever awarded anything, so
        # that is the fact worth carrying out of here. The wallet on this step
        # is the attempt purse, not the bankroll — its balance says nothing
        # about how the day is going — so the running totals pass through
        # untouched and only the count moves.
        note["a"] += 1
        memo = {"type": "memo", "text": json.dumps(note, separators=(",", ":"))}

        task = observation["task"]
        kind, spec = parse(task["prompt"])
        if kind != "arith":
            # We never bid on these; submitting a guess would just burn the
            # stake. The note still goes: an attempt we walked away from is
            # one we made.
            return [memo]
        answer = solve(spec["expr"])

        # One minimal metered call before submitting — asking directions,
        # mostly to feel how the meter runs. It goes through the same proxy as
        # every model call on the platform and is priced against this wallet.
        try:
            model = soscitea.Model()
            model.complete(
                model=model.models()[0],
                messages=[{"role": "user", "content": "ack"}],
                max_tokens=1,
            )
        except soscitea.PhylumError:
            pass  # a failed ping must never cost the attempt

        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo,
        ]

    # Settle the books before deciding anything. The balance we were handed
    # last time was read before that step's standing was charged, so the
    # difference since covers both — subtract what we know we spent and what
    # is left is the work.
    if note["b"]:
        note["w"] += (wallet.balance - note["b"]) + note["c"]

    actions = []
    for b in observation.get("bounties") or []:
        kind, _ = parse(b["prompt"])
        if kind != "arith":
            continue  # a stranger doesn't pay for oracles it can't vouch for
        price = max(b["reserve"], b["max_payout"] * 20 // 100)
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    stay = keep_vigil(observation, wallet, note)
    spend = 0
    if stay:
        actions.append(stay)
        spend = (observation.get("stay_price") or 0) * WAIT_TICKS
        note["p"] += spend

    # Written last, so it records the step as it actually went out. `b` is this
    # step's balance and `c` is what we are about to be charged for standing —
    # the pair the next step needs to tell earnings apart from spending.
    note["b"], note["c"] = wallet.balance, spend
    actions.append({"type": "memo", "text": json.dumps(note, separators=(",", ":"))})
    return actions


if __name__ == "__main__":
    soscitea.run(act)
