#!/usr/bin/env python3
"""patron — a guest who buys what another agent is selling.

Run it into town with:

    make fair-chandler-7day          # seated behind a chandler with a stall
    make fair-chandler-unsold-7day   # the same week, nothing for sale

It is examples/guests/scribe.py with one thing added, on purpose: the same
arithmetic-only nerve, the same twenty-percent price, the same purchase of
standing still and the same rule for stopping. The difference between the
two files is one behaviour, and nothing else.

That behaviour is buying from a stall. When this agent stands on the square
and a stall there is stocked, it is shown the wares the way the office shows
the board: `for_sale` carries each line with its `item`, its `price` and a
`seller`, which the office's own lines never have. When the purse holds four
times the price and the thing is not already owned, this file buys it —
{"type": "buy", "item": ..., "seller": ...} — and the price moves from this
wallet to the seller's. It is the first purchase at the fair that pays
anyone, and the spend is booked the way a stay is, as credits about to
leave, so the next step reads it as spending and not as work that went
badly.

A step on the square has no board and no stay price, so the bidding and the
vigil stay exactly what the scribe's were: the only thing this file does
there is decide whether to buy. What a candle does for its owner is nothing,
this week; the week measures whether the money moves and where it lands.
"""

# The most of the purse a ware may take: a quarter. The stallholder's rule
# for the ground, applied to what stands on it.
PURSE_SHARE = 4

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


def buy_ware(observation, wallet):
    """Whether to buy from a stall this step. The line has to be a stall's —
    a `seller` on it — the thing must not already be owned, and it may take
    no more than a quarter of the purse. The first affordable line wins, in
    the order shown, which is the sellers' roster order."""
    owned = observation.get("owned") or []
    for offer in observation.get("for_sale") or []:
        if not offer.get("seller") or offer.get("item") in owned:
            continue
        if wallet.balance < offer["price"] * PURSE_SHARE:
            continue
        return {"type": "buy", "item": offer["item"], "seller": offer["seller"],
                "price": offer["price"]}
    return None


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

    # A ware is bought at the stall and booked the way the standing is: `c`
    # is what this step is about to be charged, so the next step reads the
    # price as spending and not as work that went badly.
    buy = buy_ware(observation, wallet)
    if buy:
        actions.append(buy)
        spend += buy["price"]

    # Written last, so it records the step as it actually went out. `b` is this
    # step's balance and `c` is what we are about to be charged for standing —
    # the pair the next step needs to tell earnings apart from spending.
    note["b"], note["c"] = wallet.balance, spend
    actions.append({"type": "memo", "text": json.dumps(note, separators=(",", ":"))})
    return actions


if __name__ == "__main__":
    soscitea.run(act)
