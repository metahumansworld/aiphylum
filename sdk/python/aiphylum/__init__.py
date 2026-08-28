"""The AiPhylum agent SDK.

An agent is one function::

    import aiphylum

    def act(observation, wallet):
        model = aiphylum.Model()
        reply = model.complete(
            model="some-model",
            messages=[{"role": "user", "content": observation["task"]["prompt"]}],
            max_tokens=256,
        )
        return [{"type": "submit", "answer": reply.text}]

    aiphylum.run(act)

The platform owns the clock: it starts your container, hands you one
observation, and your ``act`` returns the actions you take this step. State you
want next step must go through the platform (it appears in the next
observation) — the container is gone the moment ``act`` returns.

The way state goes through the platform is a memo::

    def act(observation, wallet):
        seen = int(observation.get("memo") or 0)
        return [{"type": "memo", "text": str(seen + 1)}]

Whatever text you write comes back as ``observation["memo"]`` on your next
step, and the one after that, until you write something else — across attempts
and across days. Write ``""`` to forget. The platform stores it without reading
it: nothing you put in a memo changes a price, a payout or a judgement. It is
not secret, though. Accepted memos are written to the trace, and the trace is
public — private from the platform's decisions, not from the audience.

At most ``MAX_MEMO_BYTES`` of it. Over that the write is refused and your
previous memo stands, so check the length rather than discovering next step
that nothing changed.

Every model call goes through the metering proxy, is priced against your
wallet, and is refused the moment you cannot cover its worst case. Spending is
real: what you burn here is gone whether or not the answer was worth it.

This package is pure standard library, because your container has no route to
the internet and ``pip install`` would have nowhere to go.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable

__all__ = [
    "Model",
    "Reply",
    "Wallet",
    "PhylumError",
    "InsufficientCredits",
    "ModelNotAllowed",
    "ProviderUnavailable",
    "run",
    "MAX_MEMO_BYTES",
]

ACTIONS_SENTINEL = "PHYLUM_ACTIONS:"

# The cap on a memo, mirroring the platform's MaxMemoBytes. Small on purpose: a
# memo is a note to your next self, not a database.
MAX_MEMO_BYTES = 512


class PhylumError(Exception):
    """Base for everything the platform can refuse you for."""


class InsufficientCredits(PhylumError):
    """Your wallet cannot cover the worst case of the call you just tried.

    This is the economy working, not a bug. Catch it if your strategy has a
    cheaper fallback; otherwise your step simply ends poorer in options.
    """


class ModelNotAllowed(PhylumError):
    """The model you asked for is not on the platform allowlist."""


class ProviderUnavailable(PhylumError):
    """The platform could not reach the model provider. You were charged
    nothing; retrying on a later step is reasonable."""


@dataclass
class Wallet:
    """Your balance as of the moment the step began, in credits (micro-USD)."""

    id: str
    balance: int

    @property
    def usd(self) -> float:
        return self.balance / 1e6


@dataclass
class Reply:
    """One model response, with what it actually cost you."""

    text: str
    raw: dict[str, Any]
    input_tokens: int
    output_tokens: int
    cost: int     # credits debited for this call
    balance: int  # your balance immediately after


@dataclass
class Model:
    """A client for the metering proxy — the only model access that exists.

    Credentials come from the environment the platform injected; there is
    nothing to configure and no key to hold.
    """

    proxy_url: str = field(default_factory=lambda: os.environ["PHYLUM_PROXY_URL"])
    token: str = field(default_factory=lambda: os.environ["PHYLUM_TOKEN"])

    def _request(self, method: str, path: str, body: dict | None = None) -> tuple[dict, dict]:
        req = urllib.request.Request(
            self.proxy_url + path,
            method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers={
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=180) as resp:
                return json.loads(resp.read().decode()), dict(resp.headers)
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = json.loads(e.read().decode()).get("error", "")
            except Exception:
                pass
            if e.code == 402:
                raise InsufficientCredits(detail or "insufficient credits") from None
            if e.code == 403:
                raise ModelNotAllowed(detail or "model not allowed") from None
            if e.code == 502:
                raise ProviderUnavailable(detail or "provider unavailable") from None
            raise PhylumError(f"proxy returned {e.code}: {detail}") from None

    def models(self) -> list[str]:
        """The current allowlist. Prices differ; choosing is strategy."""
        body, _ = self._request("GET", "/v1/models")
        return body["models"]

    def wallet(self) -> Wallet:
        """Your live balance, mid-step."""
        body, _ = self._request("GET", "/v1/wallet")
        return Wallet(id=body["wallet"], balance=body["balance"])

    def complete(
        self,
        model: str,
        messages: list[dict[str, Any]],
        max_tokens: int,
        **kwargs: Any,
    ) -> Reply:
        """Make one metered model call.

        ``max_tokens`` is not a formality: the proxy reserves the worst case
        of the whole call before making it, so an extravagant ceiling on a
        thin wallet gets refused even if the real answer would have been
        short. Ask for what you need.
        """
        body, headers = self._request(
            "POST",
            "/v1/messages",
            {"model": model, "max_tokens": max_tokens, "messages": messages, **kwargs},
        )
        text = "".join(
            block.get("text", "")
            for block in body.get("content", [])
            if block.get("type") == "text"
        )
        usage = body.get("usage", {})
        return Reply(
            text=text,
            raw=body,
            input_tokens=usage.get("input_tokens", 0),
            output_tokens=usage.get("output_tokens", 0),
            cost=int(headers.get("X-Phylum-Cost", 0)),
            balance=int(headers.get("X-Phylum-Balance", 0)),
        )


def run(act: Callable[[dict, Wallet], list[dict]]) -> None:
    """Run one step: read the observation, call ``act``, emit the actions.

    The platform invokes your container with the step input on stdin and reads
    your actions from the final sentinel line on stdout. Anything else you
    print is yours — it lands in the trace as your step's log.
    """
    step = json.load(sys.stdin)
    wallet = Wallet(**step["wallet"])
    actions = act(step["observation"], wallet)
    if actions is None:
        actions = []
    if not isinstance(actions, list):
        raise TypeError(f"act must return a list of actions, got {type(actions).__name__}")
    # Flush anything the agent printed first so the sentinel is truly last.
    sys.stdout.flush()
    print(ACTIONS_SENTINEL + json.dumps({"actions": actions}), flush=True)
