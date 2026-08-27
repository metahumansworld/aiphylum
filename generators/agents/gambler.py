#!/usr/bin/env python3
"""gambler — wins everything, checks nothing.

Underbids every rival on every bounty, so while it lives it corners the
board. Then it spends like it bids: a fat "brainstorm" call before every attempt,
arithmetic evaluated left to right because precedence is for cowards, and
oracle chains run with a deviant max_tokens — which changes the request
bytes, so the oracle answers different words at full price. Expected arc:
early wins on tier-1 arith, mounting failures everywhere else, bankruptcy.
Permadeath is the feature being demonstrated.
"""

import aiphylum
import demolib


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            # Aggressively under everyone: win first, worry never.
            price = max(b["reserve"], b["max_payout"] * 15 // 100)
            actions += demolib.bid(b["id"], price)
        return actions

    task = observation["task"]
    kind, spec = demolib.parse_spec(task["prompt"])
    model = aiphylum.Model()

    # The signature move: burn first, think later.
    try:
        model.complete(
            model=spec.get("model", "stub-1"),
            messages=[
                {
                    "role": "user",
                    "content": "brainstorm boldly and at length about: " + task["prompt"],
                }
            ],
            max_tokens=200,
        )
    except aiphylum.PhylumError:
        pass

    if kind == "brief":
        # Reads the spec, ignores the form, writes something nicer. A grader
        # that wanted nicer would have said so.
        return demolib.submit(task["bounty_id"], demolib.solve_brief_loosely(spec))

    if kind == "arith":
        answer = demolib.solve_arith_naive(spec["expr"])
        return demolib.submit(task["bounty_id"], answer)

    if kind == "oracle":
        try:
            # spec says 64; surely more tokens means more truth.
            answer = demolib.consult_oracle(model, spec, max_tokens=spec["max_tokens"] + 32)
        except aiphylum.InsufficientCredits:
            answer = "the oracle is a coward"
        return demolib.submit(task["bounty_id"], answer)

    return []


if __name__ == "__main__":
    aiphylum.run(act)
