#!/usr/bin/env python3
"""scholar — pays for knowledge, carefully.

Bids on everything at a moderate price. Oracle bounties are solved the only
way they can be — running the prescribed chain through the proxy, at exactly
the spec's parameters. Arith it computes itself, then spends one small call
double-checking with the model. Briefs are graded by a model rather than a
key, so it does the one thing that reliably survives a grader: it writes
exactly the line the standard describes. Expected ladder shape: the biggest earner,
real but proportionate burn, high success rate.
"""

import soscitea
import demolib


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            kind, _ = demolib.parse_spec(b["prompt"])
            # Oracles are the specialty: bid keen. Arith is a sideline —
            # price it high and let the cheaper solver have it.
            pct = 30 if kind == "oracle" else 40
            if kind == "brief":
                # Cheap to produce and graded on care, which is the house
                # specialty — bid keenest of all.
                pct = 25
            price = max(b["reserve"], b["max_payout"] * pct // 100)
            actions += demolib.bid(b["id"], price)
        return actions

    task = observation["task"]
    kind, spec = demolib.parse_spec(task["prompt"])
    model = soscitea.Model()

    if kind == "oracle":
        answer = demolib.consult_oracle(model, spec)
        return demolib.submit(task["bounty_id"], answer)

    if kind == "brief":
        # No model call: the standard is in the spec, and paying a grader's
        # cousin to paraphrase it would only add ways to drift off it.
        demolib.ping(model)
        return demolib.submit(task["bounty_id"], demolib.solve_brief(spec))

    if kind == "arith":
        answer = demolib.solve_arith(spec["expr"])
        # Ask the model to confirm. The stub's opinion is noise, but the
        # habit of verifying is the strategy being modeled — and its cost
        # lands in the burn column where the ladder can judge it.
        try:
            model.complete(
                model=spec.get("model", "stub-1"),
                messages=[
                    {"role": "user", "content": "check: %s = %d ?" % (spec["expr"], answer)}
                ],
                max_tokens=16,
            )
        except soscitea.PhylumError:
            pass
        return demolib.submit(task["bounty_id"], answer)

    return []


if __name__ == "__main__":
    soscitea.run(act)
