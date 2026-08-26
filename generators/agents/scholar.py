#!/usr/bin/env python3
"""scholar — pays for knowledge, carefully.

Bids on everything at a moderate price. Oracle bounties are solved the only
way they can be — running the prescribed chain through the proxy, at exactly
the spec's parameters. Arith it computes itself, then spends one small call
double-checking with the model. Expected ladder shape: the biggest earner,
real but proportionate burn, high success rate.
"""

import dungeon
import demolib


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            kind, _ = demolib.parse_spec(b["prompt"])
            # Oracles are the specialty: bid keen. Arith is a sideline —
            # price it high and let the cheaper solver have it.
            pct = 30 if kind == "oracle" else 40
            price = max(b["reserve"], b["max_payout"] * pct // 100)
            actions += demolib.bid(b["id"], price)
        return actions

    task = observation["task"]
    kind, spec = demolib.parse_spec(task["prompt"])
    model = dungeon.Model()

    if kind == "oracle":
        answer = demolib.consult_oracle(model, spec)
        return demolib.submit(task["bounty_id"], answer)

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
        except dungeon.DungeonError:
            pass
        return demolib.submit(task["bounty_id"], answer)

    return []


if __name__ == "__main__":
    dungeon.run(act)
