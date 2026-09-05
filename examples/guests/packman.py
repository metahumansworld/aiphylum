#!/usr/bin/env python3
"""packman — the other costermonger.

The chapman.py move, once more: this file imports costermonger.py's act and
adds nothing, so the two guests at the fair are one program by construction,
differing in name and in the order the -guest flags are written on the
Makefile line — which is the order they see the board, and the order a tied
ask is broken in. Every card the peddlers tied on went to the front seat, on
every kind, for a week. This pair runs the same rule with the meter read:

    make fair-costermongers-7day

Whether a floor read from one seat's wallet ever parts the two asks — each
seat reads its own meter, and while every tie goes to the front only the
front seat's chains are run — is one of the things the trace is for.
"""

import aiphylum
from costermonger import act

if __name__ == "__main__":
    aiphylum.run(act)
