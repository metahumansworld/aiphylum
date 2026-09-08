#!/usr/bin/env python3
"""chandler — a guest who buys a stall on the square and puts one thing on
it for sale.

Run it into town with:

    make fair-chandler-7day          # a stall for sale at 400, and a patron
    make fair-chandler-unsold-7day   # the same week, nothing for sale

It is examples/guests/stallholder.py with one thing added, on purpose: the
same arithmetic-only nerve, the same twenty-percent price, the same purchase
of standing still, the same rule for stopping, the same purchase of ground.
The difference between the two files is one behaviour, and nothing else.

That behaviour is stocking. Once this file owns a stall — `owned` carries
"stall" — and its shelf is empty — the bid observation carries no `stocked`
— it puts one line on it: {"type": "stock", "item": "candle", "price": 100}.
From then on `stocked` carries the line back on every board, so the shelf is
stocked once and not once per step. Whoever stands on the square is shown
the line in `for_sale` with this agent's name on it as `seller`, and a buy
of it moves the price from their wallet to this one — transferred on the
same ledger the bounties settle on, not burned like the stall was.

The price is a constant, and a quarter of what the ground cost: four
patrons return the stall, one does not. Nothing here reads the trade back.
A sale lands in the balance between two bid steps, so the stopping rule's
`w` reads it as work returned, which for a stall it is — the tally counts
what the wallet gained and does not ask whether a bounty or a customer paid
it. The week measures whether one patron at this price earns the ground
back, and the number is in the README beside the run.
"""

# What the shelf holds and what it asks. One line, one price, never runs out
# and never changes: the week is about whether the money moves, not about
# what a candle is worth.
WARE = "candle"
WARE_PRICE = 100

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

# The most of the purse a stall may take: a quarter. A pitch is worth having
# and not worth the stake — the diarist's rule for the notebook, unchanged.
PURSE_SHARE = 4


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


def buy_stall(observation, wallet):
    """Whether to buy the stall this step. It has to be for sale, we must not
    already own one, and it may take no more than a quarter of the purse."""
    if "stall" in (observation.get("owned") or []):
        return None
    for offer in observation.get("for_sale") or []:
        if offer.get("item") != "stall":
            continue
        if wallet.balance < offer["price"] * PURSE_SHARE:
            return None
        return {"type": "buy", "item": "stall", "price": offer["price"]}
    return None


def stock_shelf(observation):
    """Whether to stock the stall this step: we have to own one, and the
    shelf has to be empty. `stocked` is the platform saying what is already
    on it, which is what keeps a fresh process from stocking every step."""
    if "stall" not in (observation.get("owned") or []):
        return None
    if observation.get("stocked"):
        return None
    return {"type": "stock", "item": WARE, "price": WARE_PRICE}


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

    # The stall is bought at the same counter as the standing and booked the
    # same way: `c` is what this step is about to be charged, so the next
    # step reads the price as spending and not as work that went badly.
    buy = buy_stall(observation, wallet)
    if buy:
        actions.append(buy)
        spend += buy["price"]

    # Stocking costs nothing and books nothing: the shelf is a line, not a
    # purchase. It goes after the buy because a stall bought this step is
    # owned next step, which is when `owned` will say so.
    stock = stock_shelf(observation)
    if stock:
        actions.append(stock)

    # Written last, so it records the step as it actually went out. `b` is this
    # step's balance and `c` is what we are about to be charged for standing —
    # the pair the next step needs to tell earnings apart from spending.
    note["b"], note["c"] = wallet.balance, spend
    actions.append({"type": "memo", "text": json.dumps(note, separators=(",", ":"))})
    return actions


if __name__ == "__main__":
    soscitea.run(act)
