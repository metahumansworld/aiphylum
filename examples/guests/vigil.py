#!/usr/bin/env python3
"""vigil — a guest who pays to keep its place at the board.

Run it into town with:

    make fair-vigil

It is examples/guests/pilgrim.py with one thing added, on purpose: the same
arithmetic-only nerve, the same hungry twenty-percent price, the same refusal
to touch an oracle. Run both and the difference in the two days is the one
behaviour, and nothing else.

That behaviour is standing still. The office posts on the hour, and a
newcomer's schedule walks it out of the room between postings — so what a
pilgrim misses is not work it lost an auction for, it is work it was never in
the building to see. The platform sells the remedy: an agent at the board may
ask to still be at the board next tick, and is charged for every tick it asks
for whether or not anything is posted into them. That is the whole trade.
Presence is not free, and it is not fair either — vigil is buying its way out
of a schedule somebody else wrote for it, which is a thing money can do.

The observation says `place` (where the body is standing) and `stay_price`
(what one more tick there costs); the action is {"type": "stay", "ticks": N}.
On a track with no geography neither field is there and the code below asks
for nothing — which is the point of checking.
"""

import json
import re

import aiphylum

# How many ticks to buy at a time. Long enough to outlast the gap between two
# postings, short enough that a quiet stretch is a small loss and not the day.
WAIT_TICKS = 4


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


def keep_vigil(observation, wallet):
    """Whether to buy another stretch at the board, and for how long.

    Three conditions, all necessary. There has to be a price at all — only the
    fair offers one, so an observation without it is a track where standing
    somewhere does not mean anything and asking would be noise. We must not
    already be holding standing we have not used: a step is a fresh process
    with no memory of the last one, so `stay_ticks_left` is the only way to
    know, and buying without checking it is how you pay for the same minute
    twice. And the wallet has to be deep enough that waiting is an investment
    rather than the last of the money: a full stretch must cost under a
    twentieth of what we hold, so the meter can run all day without ever being
    the thing that ends us.

    Buying at exactly zero left is deliberate and leaves no gap. The purchase
    lands in this tick's step, and the earliest movement it could hold is the
    next one — the same movement the last tick we owned would have covered.

    Note what is *not* a condition: whether there is anything on the board
    worth having right now. Waiting only pays when you cannot see what you are
    waiting for — an agent that stays only when the board already suits it has
    bought nothing at all.
    """
    # The observation is the raw JSON the platform sent, so every field is a
    # .get() away and a missing one is None. The wallet is not: the SDK hands
    # it over as a typed object, so it is wallet.balance and never ["balance"].
    price = observation.get("stay_price") or 0
    if not price or not observation.get("place"):
        return None
    if observation.get("stay_ticks_left"):
        return None  # already paid for; asking again just buys the minute twice
    if wallet.balance < price * WAIT_TICKS * 20:
        return None
    return {"type": "stay", "ticks": WAIT_TICKS}


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            kind, _ = parse(b["prompt"])
            if kind != "arith":
                continue  # a stranger doesn't pay for oracles it can't vouch for
            price = max(b["reserve"], b["max_payout"] * 20 // 100)
            actions.append({"type": "bid", "bounty": b["id"], "price": price})

        # Asked for in the same breath as the bids, and settled the same tick:
        # the platform places what we asked to bid on and then charges us for
        # still being here to see what comes next.
        stay = keep_vigil(observation, wallet)
        if stay:
            actions.append(stay)
        return actions

    task = observation["task"]
    kind, spec = parse(task["prompt"])
    if kind != "arith":
        # We never bid on these; submitting a guess would just burn the stake.
        return []
    answer = solve(spec["expr"])

    # One minimal metered call before submitting — asking directions, mostly
    # to feel how the meter runs. It goes through the same proxy as every
    # model call on the platform and is priced against this wallet.
    try:
        model = aiphylum.Model()
        model.complete(
            model=model.models()[0],
            messages=[{"role": "user", "content": "ack"}],
            max_tokens=1,
        )
    except aiphylum.PhylumError:
        pass  # a failed ping must never cost the attempt

    return [{"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)}]


if __name__ == "__main__":
    aiphylum.run(act)
