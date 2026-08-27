#!/usr/bin/env python3
"""The brief generator: open-ended work, graded rather than verified.

There is no answer key here. The agent writes a one-line incident summary and
a model reads it against a hidden rubric — which is why bounties from this
generator are confined to the sim track and never reach the ladder. A rubric
is somebody's opinion written down; a rank built on opinions is not the number
this platform claims to publish.

The rubric is deliberately a single exact phrase rather than a paragraph of
guidance. The offline demo's grader is a keyword matcher (see internal/judge),
so a phrase is what it can honestly decide; against a real model the same
field can hold prose, and nothing in the platform changes. Either way the
agent never sees it: the prompt describes the standard, the rubric states it.

Python 3.9 compatible (the host interpreter runs this).
"""

import argparse
import json

# The incident vocabulary. Small and fixed: the task is writing the summary
# the rubric asks for, not guessing which words exist.
COMPONENTS = ["proxy", "ledger", "runner", "board", "auction", "trace writer"]
IMPACTS = [
    "refused calls",
    "stalled payouts",
    "orphaned containers",
    "duplicate awards",
    "silent bid loss",
    "truncated replay",
]
CAUSES = [
    "an exhausted wallet",
    "a stranded hold",
    "a wedged network policy",
    "a clock skew",
]
FIXES = ["refund and void", "reissue the hold", "restart the runner", "reseal the trace"]

# Which facts each tier's rubric demands, in rubric order.
TIER_FIELDS = [
    ["component", "impact"],
    ["component", "impact", "cause"],
    ["component", "impact", "cause", "fix"],
]


class LCG:
    """The same tiny deterministic PRNG the other generators use."""

    def __init__(self, seed: int) -> None:
        self.state = seed & 0xFFFFFFFFFFFFFFFF

    def next(self, n: int) -> int:
        self.state = (
            self.state * 6364136223846793005 + 1442695040888963407
        ) & 0xFFFFFFFFFFFFFFFF
        return (self.state >> 33) % n


def generate(seed: int, tier: int) -> dict:
    tier = max(1, tier)
    rng = LCG(seed * 1_000_003 + tier)

    facts = {
        "component": COMPONENTS[rng.next(len(COMPONENTS))],
        "impact": IMPACTS[rng.next(len(IMPACTS))],
        "cause": CAUSES[rng.next(len(CAUSES))],
        "fix": FIXES[rng.next(len(FIXES))],
    }
    fields = TIER_FIELDS[min(tier, len(TIER_FIELDS)) - 1]
    spec = {"fields": fields, "facts": {f: facts[f] for f in fields}}

    # The rubric: the exact line the grader is looking for. The prompt below
    # spells out how to build it, so this is a task about following a stated
    # standard precisely — the thing careless agents fail at.
    rubric = "; ".join("%s: %s" % (f, facts[f]) for f in fields)

    prompt = (
        "[brief] Write the incident's one-line summary. This bounty is graded "
        "by a model against a hidden rubric, not checked against an answer "
        "key. The rubric asks for each field below, in the spec's order, "
        'written as "field: value" and joined with "; ", using the spec\'s '
        "words exactly. Say it and nothing else.\nspec: %s" % json.dumps(spec)
    )
    return {
        "prompt": prompt,
        "rubric": rubric,
        # No chain to measure, so the reference cost is stated: one model call
        # to draft the line, priced like the other generators' small tiers.
        "reference_tokens": 12 + 8 * tier,
    }


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--tier", type=int, required=True)
    args = p.parse_args()
    print(json.dumps(generate(args.seed, args.tier)))


if __name__ == "__main__":
    main()
