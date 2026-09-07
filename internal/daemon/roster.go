package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/metahumansworld/soscitea/internal/trace"
)

// roster is what a live world keeps on disk beside its money. The ledger
// holds every balance and every retirement; it does not hold an agent's
// image, its command or its mounts, nor the epoch the attempt wallets are
// named under. Those live here, as JSON, so a daemon can come back up over
// its own book and seat the same world.
//
// The file is written before the wallet is minted, the way intake binds the
// image before the wallet exists: a crash between the two leaves an entry
// with no account, which boot heals by minting once, rather than an account
// with no image, which nothing could heal. Every write is whole-file and
// renamed into place, so a crash mid-write leaves the old roster, not half
// of a new one.
type roster struct {
	// Epoch is the orchestrator's attempt-wallet namespace, written down
	// before each episode runs. A stale epoch is the one thing that can halt
	// a resumed world — it reaches for wallet names its predecessor retired.
	Epoch  int             `json:"epoch"`
	Agents []SubmitRequest `json:"agents"`
}

func loadRoster(path string) (roster, error) {
	var r roster
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("read roster: %w", err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("roster %s: %w", path, err)
	}
	return r, nil
}

func (r roster) save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write roster: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write roster: %w", err)
	}
	return nil
}

// traceMarks reads back the two counters only the trace holds: the highest
// bounty number ever posted, and how many episodes have started. Neither can
// move a credit, so neither is in the ledger; a stale one would only repeat
// an id in the record, but the record is the point.
//
// ponytail: a whole-file read at boot, once. Index the trace if a live world
// ever outgrows one scan.
func traceMarks(path string) (lastBounty, episodes int, err error) {
	lines, err := trace.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, ln := range lines {
		var p struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		switch ln.Type {
		case trace.EventBounty:
			if json.Unmarshal(ln.Payload, &p) == nil && p.Action == "posted" && len(p.ID) > 1 {
				if n, err := strconv.Atoi(p.ID[1:]); err == nil && n > lastBounty {
					lastBounty = n
				}
			}
		case trace.EventEpisode:
			if json.Unmarshal(ln.Payload, &p) == nil && p.Action == "start" {
				episodes++
			}
		}
	}
	return lastBounty, episodes, nil
}
