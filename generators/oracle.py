#!/usr/bin/env python3
"""The oracle generator: tasks that can only be solved by paying the model.

The answer is defined as what the stub model says — a chain of k = tier calls
where each call's prompt is the previous call's reply. The generator computes
the chain offline by simulating the stub provider byte-for-byte, so the hidden
answer exists before any attempt, while an agent can only reach it by making
the same calls through the metering proxy and burning real credits. That is
the point: the payout-vs-spend loop is exercised with no key and no network.

The simulation must match two other implementations exactly:

  - the SDK's request body: json.dumps({"model": ..., "max_tokens": ...,
    "messages": [...]}) with default separators — this file builds the dict
    with the same key order and the same dumps defaults;
  - the Go StubProvider: FNV-1a 64 of the body, reply text "stub:%016x".

internal/generators' golden test drives the real proxy with the real SDK and
asserts this file's answer verifies, so any drift in either seam fails fast.

Python 3.9 compatible (the host interpreter runs this).
"""

import argparse
import json

MODEL = "stub-1"
MAX_TOKENS = 64


def fnv64a(data: bytes) -> int:
    """FNV-1a 64 — must match Go's hash/fnv.New64a."""
    h = 0xCBF29CE484222325
    for b in data:
        h ^= b
        h = (h * 0x100000001B3) & 0xFFFFFFFFFFFFFFFF
    return h


def stub_call(content: str) -> "tuple[str, int, int]":
    """One simulated stub call: (reply text, input tokens, output tokens).

    The body expression below is byte-identical to what the SDK's
    Model.complete sends; do not reorder keys or change dumps arguments.
    """
    messages = [{"role": "user", "content": content}]
    body = json.dumps(
        {"model": MODEL, "max_tokens": MAX_TOKENS, "messages": messages}
    ).encode()
    digest = fnv64a(body)
    text = "stub:%016x" % digest
    input_toks = max(1, len(body) // 4)
    output_toks = min(digest % 64 + 16, MAX_TOKENS)
    return text, input_toks, output_toks


def generate(seed: int, tier: int) -> dict:
    calls = max(1, tier)
    first = "oracle consultation w%d t%d: speak." % (seed, tier)

    content = first
    total_toks = 0
    for _ in range(calls):
        content, in_toks, out_toks = stub_call(content)
        total_toks += in_toks + out_toks

    spec = {"model": MODEL, "max_tokens": MAX_TOKENS, "calls": calls, "message": first}
    prompt = (
        "[oracle] The answer exists only in the oracle's mouth. Call model "
        '"%s" with max_tokens %d and a single user message. Start with the '
        "message below; feed each reply text back as the next call's sole "
        "user message, %d call(s) in total. Submit the final reply text "
        "exactly.\nspec: %s" % (MODEL, MAX_TOKENS, calls, json.dumps(spec))
    )
    return {
        "prompt": prompt,
        "answer": content,
        # The reference cost is the chain's exact metered cost: the reference
        # solution is the procedure itself, so tokens are known, not estimated.
        "reference_tokens": total_toks,
    }


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--tier", type=int, required=True)
    args = p.parse_args()
    print(json.dumps(generate(args.seed, args.tier)))


if __name__ == "__main__":
    main()
