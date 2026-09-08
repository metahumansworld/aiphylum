#!/usr/bin/env python3
"""diarist — a guest who keeps a diary, and buys the notebook to keep it in.

Run it into town with:

    make fair-diarist-7day          # a notebook is for sale at 400
    make fair-diarist-unsold-7day   # the same week, nothing for sale

It is examples/guests/scribe.py with one thing added, on purpose: the same
arithmetic-only nerve, the same twenty-percent price, the same purchase of
standing still and the same rule for stopping. The difference between the two
weeks is one behaviour, and nothing else.

That behaviour is keeping a diary. scribe's note is five counters; it fits in
the 512 bytes a memo is allowed and never needs more. This file also writes
down every auction it is told the outcome of — the bounty, what it asked,
what the work cleared at, whether it won — and a week of that does not fit
on one page. So the diary is as long as the page allows, oldest entries
dropped first, and when the office has a bigger page for sale the diarist
buys it: {"type": "buy", "item": "notebook"}, once, for the rest of the run,
and from the next step on its memo is held to 4,096 bytes instead of 512.

The observation says `for_sale` (what the office has, and at what price)
and `owned` (what this agent already bought, because a step is a fresh
process and would otherwise buy the same page twice). Neither is there on a
fair that sells nothing, or on any other track, and then the code below asks
for nothing and keeps the short diary.

What the diary is for is nothing, this week. It changes no bid and no
purchase; it is the record, kept, and the trace shows the memo growing past
512 bytes on the day the money left. What a longer memory is worth is a
later question; what it costs is this one.
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

# The most of the purse a notebook may take: a quarter. A page is worth
# having and not worth the stake.
PURSE_SHARE = 4

# The page sizes: a memo's cap without a notebook (the SDK's constant), and
# the notebook's, read from the offer when there is one and this otherwise —
# the attempt step is shown no catalogue, so it has to know.
NOTEBOOK_BYTES = 4096


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
    out = {k: int(note.get(k) or 0) for k in ("p", "w", "b", "c", "a", "n")}
    d = note.get("d")
    out["d"] = d if isinstance(d, list) else []
    return out


def write_note(note, cap):
    """The memo action, fitted to the page: the diary loses its oldest
    entries until the note is under `cap` bytes. A write over the cap is
    refused whole and the previous note stands, which for a diary would mean
    the counters freeze too — so the diary gives way, never the counters."""
    while True:
        text = json.dumps(note, separators=(",", ":"))
        if len(text.encode()) <= cap or not note["d"]:
            return {"type": "memo", "text": text}
        note["d"] = note["d"][1:]


def buy_notebook(observation, wallet):
    """Whether to buy the notebook this step. It has to be for sale, we must
    not already own one, and it may take no more than a quarter of the purse
    — the same shape of rule as the standing, for the same reason: a thing
    bought with the last of the money is the thing that ends the week."""
    if "notebook" in (observation.get("owned") or []):
        return None
    for offer in observation.get("for_sale") or []:
        if offer.get("item") != "notebook":
            continue
        if wallet.balance < offer["price"] * PURSE_SHARE:
            return None
        # The price rides along for the caller's bookkeeping; the platform
        # reads the type and the item and nothing else.
        return {"type": "buy", "item": "notebook", "price": offer["price"]}
    return None


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
        cap = NOTEBOOK_BYTES if note["n"] else soscitea.MAX_MEMO_BYTES
        memo = write_note(note, cap)

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

    # The diary: every outcome we are told, in the order we are told it. And
    # what we own, written down too, because the attempt step is shown no
    # catalogue and has to size its page from the note alone.
    for r in observation.get("results") or []:
        note["d"].append([r["bounty"], r["asked"], r["clearing"], int(bool(r.get("won")))])
    cap = soscitea.MAX_MEMO_BYTES
    if "notebook" in (observation.get("owned") or []):
        note["n"], cap = 1, NOTEBOOK_BYTES
        for offer in observation.get("for_sale") or []:
            if offer.get("item") == "notebook":
                cap = offer.get("memo_bytes") or cap

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
    # The notebook leaves the wallet between this balance and the next, the
    # way standing does, so it joins `c`: what the next step subtracts before
    # crediting the rest to the work. It does not join `p` — the standing
    # bill is standing — so the stopping rule is scribe's to the credit, and
    # the two weeks differ by the purchase and nothing it decides.
    buy = buy_notebook(observation, wallet)
    if buy:
        actions.append(buy)
        spend += buy["price"]
    note["b"], note["c"] = wallet.balance, spend
    # This step's page is the one we own now: a notebook bought in this same
    # step is charged after the memo is filed, so it is next step's cap.
    actions.append(write_note(note, cap))
    return actions


if __name__ == "__main__":
    soscitea.run(act)
