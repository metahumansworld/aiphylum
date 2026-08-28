#!/usr/bin/env python3
"""haggler — a guest who finds the price by getting it wrong in both directions.

Run it into town with:

    make fair-haggle

It is examples/guests/pilgrim.py with one thing added, on purpose: the same
arithmetic-only nerve, the same refusal to touch an oracle it cannot vouch for.
What changes is the number it writes on the card.

pilgrim asks twenty percent of the posted maximum, every time, forever. That
number is a guess made before it had ever seen the market, and nothing that
happens afterwards can move it — not because pilgrim is stubborn but because it
has no way to notice. It bids, and then it is either given work or it is not,
and those two outcomes are the whole of what a bid step used to be able to see.
Losing an auction and watching a bounty go unsold looked identical from the
inside: a board with the same card still pinned to it.

The platform now says what happened. Every auction you bid in comes back once,
at your next bid step, in observation["results"]:

    {"bounty": "b0004", "round": 33, "asked": 220,
     "clearing": 150, "winner": "gambler", "bidders": 3}

Your own ask, whether you won, what the work actually went for, who took it,
and how thick the bidding was — and that one is a loss, which is why it has no
"won" key: a false flag is left out of the wire entirely, absence is the loss,
and a win says "won": true. Reach for it with .get, never with an index. What
you are never handed is the rest of the book — that is in the trace for the
audience, not in your observation, because an agent handed every rival's exact
number is reading strategies rather than learning a price. Learn it the way
you would have to learn it anywhere: by being wrong about it.

That leaves an asymmetry this file is built around, and it is a real property
of sealed first-price auctions rather than a quirk of the implementation:

    losing tells you the price. Winning tells you nothing.

A loss reports someone else's clearing price, which is a fact about the market.
A win reports your own ask back to you, because you were the lowest and by how
much is exactly what a sealed auction refuses to say. So an agent that only
learns from losses ratchets its price down forever and arrives at the reserve
floor, having taught itself to work for nothing. The only way to find the top
of the range is to walk into it: after a win, ask for more.

Hence the two rules, and they are the whole strategy:

    lost   -> ask under what it cleared at. You now know a price that wins.
    won    -> ask more than you did. You may have been leaving money behind.

Which is a haggler. It overshoots, it undershoots, and it circles the number
nobody will tell it.

It also has a floor, and the floor is where the rule runs out. Pinned at
MIN_PCT there is nothing left to undercut with: the ask clamps back to the same
percentage, the bid comes out at exactly the price that just won, and a tie goes
to whoever arrived first — so the memo reads "lost ... at 365, cleared 365" and
means it. Watch fair-haggle and you will see it happen two rounds running.
The rule is not wrong, it is just out of room, and knowing which of those it is
is the reason the note keeps a sentence and not only a number.

The memo is what makes this possible at all. A step is a fresh process; results
arrive once and are gone. Milestone by milestone that is the point: the memo
gave an agent somewhere to put a thought, and the results gave it a thought
worth putting there. Neither is much use alone.
"""

import json
import re

import aiphylum

# The opening ask, as a percentage of the posted maximum. pilgrim's number, so
# the first bid of the day is the bid pilgrim would have made and every
# difference after it is something this file learned.
OPENING_PCT = 20

# How far to move on each lesson, as a percentage of the current ask.
#
# Down is the smaller step because a loss hands over a price that is known to
# have won, and stepping just under it is enough; up is larger because a win
# hands over nothing at all, so the only way to discover the ceiling is to
# probe for it and a timid probe never arrives. Both are judgements rather than
# facts, written as constants so they can be argued with.
UNDERCUT_PCT = 5
PROBE_PCT = 12

# The bands the ask is kept inside, as percentages of the posted maximum. The
# floor is above the reserve on purpose: the reserve is what the platform will
# tolerate, not what the work is worth, and an agent that treats a floor as a
# target has stopped haggling and started surrendering.
MIN_PCT = 15
MAX_PCT = 60


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

    A memo written by an older version of this file — or by nothing at all, on
    the first step of the first day — has to be handled rather than assumed
    away. Anything unreadable is treated as no memory: starting over is wrong
    but survivable, where crashing on the first step is neither.

    Two numbers and a note are kept:

        pct  the current ask, as a percentage of each bounty's posted maximum,
             which is what makes one lesson transfer to a bounty of a different
             size. A price learned on a 240-credit card is worthless as a
             number and useful as a ratio.
        n    lessons taken.
        why  the last one, in words, for whoever is reading the feed.

    Two numbers and a sentence, well under MAX_MEMO_BYTES — which matters,
    because over the cap the write is refused and the previous note stands,
    and an agent that silently stops learning is worse than one that never
    started.
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
    pct = int(note.get("pct") or 0) or OPENING_PCT
    return {
        "pct": max(MIN_PCT, min(MAX_PCT, pct)),
        "n": int(note.get("n") or 0),
        "why": str(note.get("why") or "")[:120],
    }


def haggle(note, results):
    """Move the ask on what the last round's auctions taught.

    Results arrive one per auction, so a step that bid on four bounties can be
    told four things at once. They are folded in order and each one moves the
    ask, which means a step that lost three and won one lands somewhere in
    between — the right behaviour, since all four were priced the same way.

    The last lesson is kept in the note under "why". It costs a few dozen bytes
    of the memo allowance and it is the only way anyone watching can see this
    working: results are never traced (they are derivable from the award, so a
    line per bidder would only repeat it), which leaves the agent's own memo as
    the record of what it made of them.
    """
    pct = note["pct"]
    for r in results:
        asked, clearing = int(r.get("asked") or 0), int(r.get("clearing") or 0)
        if r.get("won"):
            # Won at our own ask, which says we were lowest and refuses to say
            # by how much. Ask for more and find out where the edge is.
            nxt = pct + max(1, pct * PROBE_PCT // 100)
            lesson = "won %s at %d" % (r.get("bounty"), asked)
        elif asked:
            # Lost, and told the price that beat us. Aim just under it, as a
            # ratio rather than a number: the next card will be a different
            # size, and 150 credits does not travel from a 1000-credit bounty
            # to a 240-credit one while "fifteen percent" does.
            #
            # We asked pct% of the maximum, so clearing is (clearing/asked) of
            # what we asked, and therefore that same share of pct.
            won_pct = clearing * pct // asked
            nxt = won_pct - max(1, won_pct * UNDERCUT_PCT // 100)
            lesson = "lost %s at %d, cleared %d" % (r.get("bounty"), asked, clearing)
        else:
            continue  # a result we cannot price against teaches nothing
        # Clamp first, then write it down. The note has to say the ask that was
        # actually adopted, not the one the rule proposed before the bands got
        # to it, or the feed reports a percentage nobody ever bid.
        pct = max(MIN_PCT, min(MAX_PCT, nxt))
        note["why"] = "%s, try %d%%" % (lesson, pct)
        note["n"] += 1
    note["pct"] = pct


def act(observation, wallet):
    note = read_note(observation)

    if observation["phase"] != "bid":
        # The attempt phase, which teaches nothing about prices: the auction is
        # already over and this wallet is the attempt purse, not the bankroll.
        # The note passes through untouched so the next bid step reads exactly
        # what the last one wrote.
        memo = {"type": "memo", "text": json.dumps(note, separators=(",", ":"))}
        task = observation["task"]
        kind, spec = parse(task["prompt"])
        if kind != "arith":
            return [memo]
        answer = solve(spec["expr"])

        # One minimal metered call before submitting — asking directions,
        # mostly to feel how the meter runs. It goes through the same proxy as
        # every model call on the platform and is priced against this wallet.
        try:
            model = aiphylum.Model()
            model.complete(
                model=model.models()[0],
                messages=[{"role": "user", "content": "ack"}],
                max_tokens=1,
            )
        except aiphylum.PhylumError:
            pass  # a failed ping must never cost the attempt

        return [
            {"type": "submit", "bounty": task["bounty_id"], "answer": str(answer)},
            memo,
        ]

    # Learn before bidding: last round's outcomes are handed over exactly once,
    # here, and are gone the moment this step ends.
    haggle(note, observation.get("results") or [])

    actions = []
    for b in observation.get("bounties") or []:
        kind, _ = parse(b["prompt"])
        if kind != "arith":
            continue  # a stranger doesn't pay for oracles it can't vouch for
        price = max(b["reserve"], b["max_payout"] * note["pct"] // 100)
        actions.append({"type": "bid", "bounty": b["id"], "price": price})

    actions.append({"type": "memo", "text": json.dumps(note, separators=(",", ":"))})
    return actions


if __name__ == "__main__":
    aiphylum.run(act)
