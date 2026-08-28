#!/usr/bin/env python3
"""rival — haggler again, under a different name, standing at the same board.

Run them both into town with:

    make fair-rivals

There is no strategy in this file. There is one import:

    from haggler import act

and that is the entire point. The README argues for sealing the auction book by
saying what an open one would do — hand every agent every rival's exact number
each round and

    Let them price off each other for long enough and nobody is pricing off the
    work: the reserve stops being the floor it was built as and becomes the
    place everybody ends up standing.

That was written from the shape of the rule rather than from a run, and nothing
in the repo had ever put two learners at one board to see what the *sealed*
book does under the same pressure. This file is that control. If rival held a
copy of haggler's strategy the control would be worth less: two files drift,
and a difference in outcome could always be a difference in code nobody
noticed. Importing makes them identical the way arithmetic is identical.
Whatever separates them in the trace is not the strategy, because there is only
one strategy.

What does separate them is everything the platform knows about them and they do
not know about each other:

    the name           "rival" is not "haggler", and ids are the join key for
                       memos, wallets and ledger accounts, so each keeps its own
                       memory of what it thinks a fair price is.
    the arrival order  both stand in the office in the same hours, so both are
                       shown the same board in the same tick — but Visit walks
                       the standings in roster order, roster order is the cast
                       and then the guests in the order the -guest flags were
                       written, and the auction breaks a tie on arrival. haggler
                       is the first guest flag in the Makefile line. That is the
                       whole of its advantage over rival, and the cast's whole
                       advantage over the pair of them.

The second one is the one to watch, because it is the one that decides. Three
days of this and the price stops moving: both converge on MIN_PCT — haggler's
own floor, three times the platform's reserve — and stay, since the rule
undercuts a clearing price and MIN_PCT is not one. The whole second agent saves
the poster two credits. What it changes is the queue. Thirteen auctions had
both names in the book and seven of those were exact ties, and rival lost every
one of them: five to haggler, and two to gambler, which was level at the same
365 on a three-way tie and is cast rather than guest. rival won once in three
days, and not by winning a tie — haggler had just won something and probed
upward out of the way, leaving it alone at the cheapest ask by two credits.
Same program, same schedule, same board: 1,228 credits and 36.

Which is the fair's founding argument arriving from the other direction. Who
was standing at the board when a bounty appeared is a schedule, not a skill — a
price war ends by taking price out of the decision, and what is left deciding
is the schedule. The mechanism does not have an opinion about which of those it
is doing.

A guest is registered under the name of its file, so a second copy of a
strategy needs a second filename — cmd/phylumd/guest.go refuses the same file
twice, on purpose, because two agents with one name is a ledger bug waiting to
be found in the dark. And the guest's own directory is on PYTHONPATH so it can
split itself into modules; nothing says the modules have to be its own.

See examples/guests/haggler.py for the strategy, the memo format, and the
asymmetry both of these are built on: losing tells you the price, winning tells
you nothing.
"""

import aiphylum

from haggler import act

if __name__ == "__main__":
    aiphylum.run(act)
