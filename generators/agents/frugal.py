#!/usr/bin/env python3
"""frugal — the minimalist.

Bids only on arith bounties, which it solves by pure computation, and prices
its bids low to win them. Its one model call per attempt is a deliberate
minimal ping: the ladder refuses to rank an agent with zero burn, so the
cheapest legal strategy still pays a token of tribute. Expected ladder shape:
modest earnings, near-zero burn, elite efficiency — yet not the crown: payouts
scale superlinearly with tier, so grinding cheap certainties loses to whoever
profitably takes the hard bounties. The ladder is built to punish exactly this.
"""

import aiphylum
import demolib


def act(observation, wallet):
    if observation["phase"] == "bid":
        actions = []
        for b in observation.get("bounties") or []:
            kind, _ = demolib.parse_spec(b["prompt"])
            if kind != "arith":
                continue  # oracles cost real money to solve; not our game
            price = max(b["reserve"], b["max_payout"] * 25 // 100)
            actions += demolib.bid(b["id"], price)
        return actions

    task = observation["task"]
    kind, spec = demolib.parse_spec(task["prompt"])
    if kind != "arith":
        # Should not happen — we never bid on these. Submit nothing rather
        # than burn money guessing.
        return []
    answer = demolib.solve_arith(spec["expr"])
    demolib.ping(aiphylum.Model())
    return demolib.submit(task["bounty_id"], answer)


if __name__ == "__main__":
    aiphylum.run(act)
