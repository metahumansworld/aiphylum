#!/usr/bin/env python3
"""pilgrim — a worked example of a guest: an agent you wrote, at the fair.

Run it into town with:

    go run ./cmd/phylumd -fair -guest examples/guests/pilgrim.py

or, with its own trace so the pinned one stays pinned:

    make fair-guest

The filename becomes the agent's name — its wallet, its walker, its line in
the feed. The town gives it the rest: a room at the Bell & Bushel and a
newcomer's daily round that puts it at the Bounty Office for every posting
but the one o'clock. You author the trader; the town authors the body.

The pilgrim's strategy is a newcomer's: it only takes work it can check with
its own hands (arithmetic), and it prices hungry — twenty percent of the
payout, under the locals — because a stranger with no reputation buys its
first customers. Everything else on the board it lets pass; oracles cost
money it doesn't understand yet.

This file imports nothing but the SDK and the standard library, on purpose:
it is the whole of what a guest needs.
"""

import json
import re

import soscitea


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


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            kind, _ = parse(b["prompt"])
            if kind != "arith":
                continue  # a stranger doesn't pay for oracles it can't vouch for
            price = max(b["reserve"], b["max_payout"] * 20 // 100)
            actions.append({"type": "bid", "bounty": b["id"], "price": price})
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
        model = soscitea.Model()
        model.complete(
            model=model.models()[0],
            messages=[{"role": "user", "content": "ack"}],
            max_tokens=1,
        )
    except soscitea.PhylumError:
        pass  # a failed ping must never cost the attempt

    return [{"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)}]


if __name__ == "__main__":
    soscitea.run(act)
