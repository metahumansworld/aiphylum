#!/usr/bin/env python3
"""Golden-test driver: solve an oracle prompt exactly the way scholar does.

Reads the bounty prompt on stdin, runs the chain through the real proxy via
the real SDK, prints the final answer. The Go test asserts this verifies
against the generator's precomputed hidden answer — the cross-language pin
for the whole oracle seam (SDK bytes == generator simulation == StubProvider).
"""

import sys

import aiphylum
import demolib


def main() -> None:
    _, spec = demolib.parse_spec(sys.stdin.read())
    print(demolib.consult_oracle(aiphylum.Model(), spec))


if __name__ == "__main__":
    main()
