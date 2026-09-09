// Guest intake for the fair: the -guest flag names a Python file a user wrote
// against the SDK, and this file turns it into a lodger. The division is the
// point — the flag brings the trader, town.Guest brings the body — and the
// checks here are the whole of the platform's opinion about the code it is
// handed: it must exist, it must be Python, and its name must not collide
// with anyone already living in Ashmere. What the agent does with its turn is
// its author's business; what it can spend is its wallet's.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// guestGrant is the standard newcomer's purse. Every guest gets the same one:
// the cast's grants differ because their strategies are the demo's content,
// but a guest's strategy is unknown by definition, so the only fair stake is
// a flat one.
const guestGrant = 2000

type guest struct {
	id   string // wallet and roster name, derived from the filename
	path string // absolute path to the agent's Python file
}

// guestID is the shape a filename must reduce to before it can be a wallet:
// the id ends up in trace payloads, DOM ids and ledger rows, all of which the
// cast's names already flow through, so a guest's name gets the same alphabet.
// Forbidding the colon also puts the ledger's own accounts out of reach: they
// are all namespaced sys: (ledger.AcctMint and friends), so no filename can
// reduce to one and no guest can be handed the faucet by accident.
var guestID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)

// guestRoster resolves the -guest flags into agents ready to register. taken
// holds every name the world has already given out — the cast, the residents,
// the judge — because one id reaching two owners would merge their wallets,
// their walkers and their feed lines into a single wrong story.
func guestRoster(paths []string, taken map[string]bool) ([]guest, error) {
	var out []guest
	for _, p := range paths {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, ".py") {
			return nil, fmt.Errorf("guest %s: a guest is a Python file the fair runs as python3 %s", p, base)
		}
		id := strings.ToLower(strings.TrimSuffix(base, ".py"))
		id = strings.ReplaceAll(id, "_", "-")
		if !guestID.MatchString(id) {
			return nil, fmt.Errorf("guest %s: the filename becomes the agent's name and %q is not one — use 2-16 of a-z, 0-9, hyphen, starting with a letter", p, id)
		}
		if taken[id] {
			return nil, fmt.Errorf("guest %s: the name %q is already taken in this world", p, id)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("guest %s: %w", p, err)
		}
		// Taken only once the file is real: at boot a refusal is fatal
		// either way, but through the door a mistyped path is answered and
		// the fair goes on, and the corrected knock a moment later must not
		// find the name burned by the mistake.
		taken[id] = true
		out = append(out, guest{id: id, path: abs})
	}
	return out, nil
}
