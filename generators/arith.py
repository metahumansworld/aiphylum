#!/usr/bin/env python3
"""The arith generator: tasks solvable by pure computation, no model needed.

An expression over + - * with normal precedence. Tier 1 is additions only;
tier 2 and up mix operators and always contains at least one "+ a * b" so
that naive left-to-right evaluation gets it wrong — precedence is the teeth.

An agent that can parse gets these nearly for free, which is exactly what the
efficiency ladder is supposed to reward. reference_tokens prices the payout
as if a model-based reference solved it (a real reference solution would
spend tokens even on arithmetic); it is fixed per tier so bounty economics
are deterministic.

Python 3.9 compatible (the host interpreter runs this).
"""

import argparse
import json


class LCG:
    """A tiny deterministic PRNG so results never hinge on CPython's random
    module internals staying put across versions."""

    def __init__(self, seed: int) -> None:
        self.state = seed & 0xFFFFFFFFFFFFFFFF

    def next(self, n: int) -> int:
        """Uniform-ish integer in [0, n)."""
        self.state = (
            self.state * 6364136223846793005 + 1442695040888963407
        ) & 0xFFFFFFFFFFFFFFFF
        return (self.state >> 33) % n


def generate(seed: int, tier: int) -> dict:
    tier = max(1, tier)
    rng = LCG(seed * 1_000_003 + tier)

    operands = [2 + rng.next(98) for _ in range(tier + 2)]
    if tier == 1:
        ops = ["+"] * (len(operands) - 1)
    else:
        ops = [("+", "-", "*")[rng.next(3)] for _ in range(len(operands) - 1)]
        # Guarantee the precedence trap: somewhere a +/- is followed by a *.
        ops[0] = "+" if ops[0] != "-" else "-"
        ops[1] = "*"

    parts = [str(operands[0])]
    for op, val in zip(ops, operands[1:]):
        parts.append(op)
        parts.append(str(val))
    expr = " ".join(parts)

    # The generator's own answer: evaluate with normal precedence. The
    # expression is generated from digits and + - * just above, so eval is
    # evaluating our own arithmetic, not input.
    answer = eval(expr)  # noqa: S307

    spec = {"expr": expr}
    prompt = (
        "[arith] Compute the value of the expression below, with the usual "
        "operator precedence. Submit the number.\nspec: %s" % json.dumps(spec)
    )
    return {
        "prompt": prompt,
        "answer": str(answer),
        "reference_tokens": 10 + 6 * tier,
    }


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--tier", type=int, required=True)
    args = p.parse_args()
    print(json.dumps(generate(args.seed, args.tier)))


if __name__ == "__main__":
    main()
