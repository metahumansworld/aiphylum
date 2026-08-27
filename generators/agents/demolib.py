"""Shared plumbing for the demo reference agents.

Every generator's prompt starts with a "[kind]" tag and ends with a
"spec: {...}" line — parse_spec turns that into (kind, dict). The solvers
here are the honest implementations; the agents differ in strategy (what to
bid on, what to pay for), not in arithmetic.

Python 3.9 compatible (agents run on the host interpreter in the demo).
"""

import json
import re

import dungeon


def parse_spec(prompt):
    """-> (kind, spec dict). kind is the leading [tag]; spec is the last
    'spec: {...}' line. Unknown prompts return ("", {})."""
    kind = ""
    m = re.match(r"\[(\w+)\]", prompt)
    if m:
        kind = m.group(1)
    spec = {}
    for line in prompt.splitlines():
        if line.startswith("spec: "):
            try:
                spec = json.loads(line[len("spec: "):])
            except ValueError:
                pass
    return kind, spec


def solve_arith(expr):
    """Evaluate with normal precedence. Refuses anything but digits, + - *
    and spaces, so eval sees arithmetic and nothing else."""
    if not re.fullmatch(r"[0-9+\-* ]+", expr):
        raise ValueError("not an arithmetic expression: %r" % expr)
    return eval(expr)  # noqa: S307


def solve_arith_naive(expr):
    """Left-to-right, precedence-blind — the gambler's arithmetic. Wrong
    whenever the expression mixes * after + or -, which tier 2+ always does."""
    tokens = expr.split()
    acc = int(tokens[0])
    for op, val in zip(tokens[1::2], tokens[2::2]):
        v = int(val)
        if op == "+":
            acc += v
        elif op == "-":
            acc -= v
        else:
            acc *= v
    return acc


def consult_oracle(model, spec, max_tokens=None):
    """Run the oracle chain per spec and return the final reply text.

    Every call goes through the metering proxy: this is the code path that
    burns real credits. max_tokens overrides the spec's value — deviating
    changes the request bytes, so the stub's reply diverges from the hidden
    answer; you pay full price for the wrong words (see gambler.py).
    """
    mt = spec["max_tokens"] if max_tokens is None else max_tokens
    content = spec["message"]
    for _ in range(spec["calls"]):
        reply = model.complete(
            model=spec["model"],
            messages=[{"role": "user", "content": content}],
            max_tokens=mt,
        )
        content = reply.text
    return content


def solve_brief(spec):
    """Build the summary line the brief's rubric asks for: every field in the
    spec's order, "field: value", joined with "; ". The spec states the
    standard, so an agent that reads carefully passes a grader it never sees.
    """
    return "; ".join("%s: %s" % (f, spec["facts"][f]) for f in spec["fields"])


def solve_brief_loosely(spec):
    """The gambler's summary: the same facts, written as prose. Any grader
    worth the name is looking for the stated form, so this is a submission
    that reads fine and fails — which is the point of a judged bounty."""
    facts = spec["facts"]
    parts = ["the %s had trouble" % facts["component"], "we saw %s" % facts["impact"]]
    if "cause" in facts:
        parts.append("blame %s" % facts["cause"])
    if "fix" in facts:
        parts.append("we will %s" % facts["fix"])
    return ", ".join(parts) + "."


def submit(bounty_id, answer):
    return [{"type": "submit", "bounty": bounty_id, "answer": str(answer)}]


def bid(bounty_id, price):
    return [{"type": "bid", "bounty": bounty_id, "price": int(price)}]


def ping(model):
    """One minimal metered call. The ladder gates on Burned > 0 — an agent
    that never spends a credit never ranks, so even a computation-only
    strategy must put skin in the game."""
    try:
        cheapest = model.models()[0]
        model.complete(
            model=cheapest,
            messages=[{"role": "user", "content": "ack"}],
            max_tokens=1,
        )
    except dungeon.DungeonError:
        pass  # a failed ping must never cost the attempt
