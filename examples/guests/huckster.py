#!/usr/bin/env python3
"""huckster — the haggler, handed the book.

Run it into town with:

    make fair-hucksters

It is examples/guests/haggler.py with one thing changed, on purpose: the same
arithmetic-only nerve, the same memo, the same clamped percentage walking from
card to card. What changes is where the number comes from. The haggler circles
a price nobody will tell it; the huckster is told.

Because this file only makes sense at a fair run with -book open. There, every
auction you bid in comes back with the whole book in it:

    {"bounty": "b0004", "round": 33, "asked": 220, "clearing": 150,
     "winner": "gambler", "bidders": 3,
     "book": [{"agent": "frugal", "asked": 180},
              {"agent": "gambler", "asked": 150},
              {"agent": "huckster", "asked": 220}]}

Every name and every ask, your own among them, in arrival order — not price
order, which is why the rule below reads the book with min() and never with
book[0]. Run sealed —
which is the default, and every fair before this one — there is no "book" key,
no lesson ever arrives, and the huckster bids its opening guess forever: a
pilgrim with a grudge. That is deliberate. This file has exactly one way of
learning, so that whatever it does in an open day is attributable to the book
and nothing else.

The haggler needed two rules because a sealed win teaches nothing: it probed
upward blind after every win to find a ceiling the platform refused to name.
The open book abolishes the asymmetry, so the huckster needs one rule:

    price just under the cheapest number in the book that is not yours.

Lost, the cheapest rival is whoever beat you, and just under their ask is the
bid that would have won. Won, the cheapest rival is the runner-up — the exact
margin a sealed auction exists to withhold — and just under their ask is the
same win at a better price. One rule, both directions, and no probing while
there is anything to read — an auction the huckster stood alone in is the one
case the rule cannot price, and there it probes upward the way the sealed
agents always have to. Everywhere else the book replaces discovery with
reading, which is the entire charge the protocol lays against it.

And this file has no floor of its own. The haggler keeps MIN_PCT, a price
below which it will not work, and the README shows the pair of them
resting on it — their floor, chosen, three times the platform's. The
huckster's only floor is the reserve on each card, because that is the floor
it did not choose: an agent that prices off the book has exactly one reason
left to stop, and it is the platform saying no. Wherever the open day ends up,
that is the honest place to find out.

One caveat, kept where it bites: the memo's percentage prices each bid as
pct% of the card's maximum, and the ratio arithmetic below assumes the bid it
placed was exactly that. Once the reserve clamp binds — pct% of the maximum
under the card's reserve — the assumption bends, and the note's percentage
keeps sliding after the price cannot. The bid stays pinned at the reserve
either way, which is the state the sliding describes.
"""

import json
import re

import soscitea

# The opening ask, as a percentage of the posted maximum: pilgrim's number,
# and the haggler's, so the first bid of the day is the bid either would have
# made and every difference after it is something this file read.
OPENING_PCT = 20

# How far under the cheapest rival number to go, as a percentage of it — the
# haggler's own down-step, inherited unchanged on purpose: the experiment is
# about what an agent knows, and reusing the step size keeps the knowing the
# only difference. PROBE is for the one case the book cannot help with, an
# auction you were alone in: nothing to read, so ask for more, the way the
# haggler does after a win.
UNDERCUT_PCT = 5
PROBE_PCT = 12

# The ceiling, as a percentage of the posted maximum. There is deliberately
# no MIN_PCT beside it — see the docstring: the huckster's floor is each
# card's reserve, applied where the bid is priced, because the platform's
# floor is the one this file did not get to choose.
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
    """The memo, decoded, with every field defaulted — the haggler's note
    shape, minus its floor: pct is clamped to the ceiling and to one, and one
    is bookkeeping rather than a price, since every bid is raised to the
    card's reserve on its way out."""
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
        "pct": max(1, min(MAX_PCT, pct)),
        "n": int(note.get("n") or 0),
        "why": str(note.get("why") or "")[:120],
    }


def read_book(note, me, results):
    """Move the ask on what the books said.

    Results arrive one per auction and are folded in order, the haggler's way.
    Each carries the whole book when the fair was run -book open; the cheapest
    ask in it that is not ours is the number to stand just under. As a ratio,
    not a number: we asked pct% of the card's maximum, so a rival's ask is
    (theirs/ours) of that same pct, and a ratio travels between cards of
    different sizes where 150 credits does not.

    A book with nobody else in it teaches nothing to undercut, so the ask
    probes upward instead — being alone is the one fact the book can offer
    that argues for asking more. A result with no book at all is a sealed
    fair, and teaches nothing: see the docstring, that silence is this file's
    control on itself.
    """
    pct = note["pct"]
    for r in results:
        asked = int(r.get("asked") or 0)
        rivals = [int(e.get("asked") or 0)
                  for e in (r.get("book") or [])
                  if e.get("agent") != me and int(e.get("asked") or 0) > 0]
        if not asked:
            continue
        if rivals:
            rival_pct = min(rivals) * pct // asked
            nxt = rival_pct - max(1, rival_pct * UNDERCUT_PCT // 100)
            lesson = "read %s: cheapest rival %d" % (r.get("bounty"), min(rivals))
        elif r.get("book"):
            # Alone in the book. Nobody to read, so find the ceiling the way
            # the sealed agents have to: by walking into it.
            nxt = pct + max(1, pct * PROBE_PCT // 100)
            lesson = "alone on %s at %d" % (r.get("bounty"), asked)
        else:
            continue  # a sealed result; the huckster has nothing to read
        pct = max(1, min(MAX_PCT, nxt))
        note["why"] = "%s, try %d%%" % (lesson, pct)
        note["n"] += 1
    note["pct"] = pct


def act(observation, wallet):
    note = read_note(observation)

    if observation["phase"] != "bid":
        # The attempt phase, which prices nothing: the auction is over and
        # this wallet is the attempt purse, not the bankroll. The note passes
        # through untouched so the next bid step reads what the last wrote.
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

    # Read before bidding: the books arrive exactly once, here, and are gone
    # the moment this step ends. The wallet on a bid step is the bankroll and
    # its id is this agent's name — which is how a reader of the whole book
    # knows which line of it to look away from.
    read_book(note, wallet.id, observation.get("results") or [])

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
    soscitea.run(act)
