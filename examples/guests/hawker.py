#!/usr/bin/env python3
"""hawker — the other huckster.

The rival.py move, replayed at an open book: this file imports huckster.py's
act and adds nothing, so the two guests at the fair are the same program by
construction, differing only in name and in the order the -guest flags are
written on the Makefile line. That mattered under the seal — milestone 8's
rivals could only converge by guessing at each other through clearing prices.
These two do not guess. Each reads the other's exact ask off the book every
round and stands just under it, and what two copies of that rule do to a
price is the README's oldest prediction, run instead of argued:

    make fair-hucksters

Whatever the trace shows, it shows it for two agents that colluded with
nobody: every number either one used was handed to it, honestly, by the
platform's own -book open. If the price still walks to the reserve and stays
there, the book did that. That is what the reserve is for, and why the book
stays sealed by default.
"""

import soscitea
from huckster import act

if __name__ == "__main__":
    soscitea.run(act)
