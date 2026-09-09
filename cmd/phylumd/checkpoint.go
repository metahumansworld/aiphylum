// The checkpoint: the fair written down at a tick boundary, so the world
// survives its own daemon. Two files side by side — the books, copied whole
// by the ledger, and one JSON record of everything else: the flags the week
// was started with, the tick and the trace line it stood at, the guests by
// name and file, the town and the fair. The books go down first and the
// record second, so a record that exists always has its books beside it;
// and the record is written to a temporary name and renamed into place, so
// a kill mid-write leaves the previous checkpoint rather than half of this
// one.
//
// Resume is the mirror: copy the books over the working database before the
// ledger opens, cut the trace back to the line the record names, seat the
// roster from the record without a grant, and start the town at the tick
// after. A process killed mid-tick has money and lines past the checkpoint
// in both files; both are discarded, and the resumed daemon does that tick
// again, which is why the two runs write the same record.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/orchestrator"
	"github.com/metahumansworld/soscitea/internal/town"
)

type checkpoint struct {
	// The flags that shape the week, so a resume under different ones is
	// refused rather than run as a different world under the same trace.
	Seed     int64
	Days     int
	Tiebreak string
	Book     string
	Notebook ledger.Credits
	Stall    ledger.Credits

	Tick   int
	Seq    int64 // the trace's last line when the state was taken
	Guests []guestRecord
	Town   town.State
	Fair   orchestrator.FairState
}

// guestRecord is a guest as the record keeps it: the wallet name and the
// file, which is all a guest is between steps.
type guestRecord struct {
	ID   string
	Path string
}

// booksOf is where a checkpoint's copy of the ledger lives: beside it.
func booksOf(path string) string { return path + ".db" }

func loadCheckpoint(path string) (*checkpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", path, err)
	}
	return &cp, nil
}

// matches refuses a resume whose flags disagree with the record's: the
// deck is the seed's, the days are the calendar, the policies are the
// opening line's — none of them can change mid-week.
func (cp *checkpoint) matches(opt options) error {
	same := cp.Seed == opt.seed && cp.Days == opt.days && cp.Tiebreak == opt.tiebreak &&
		cp.Book == opt.book && cp.Notebook == opt.notebook && cp.Stall == opt.stall
	if !same {
		return fmt.Errorf("-resume: the checkpoint was taken with -seed %d -days %d -tiebreak %s -book %s -notebook %d -stall %d; run it with the same",
			cp.Seed, cp.Days, cp.Tiebreak, cp.Book, cp.Notebook, cp.Stall)
	}
	return nil
}

// writeCheckpoint puts the record down atomically; the caller has already
// put the books beside it, so a record that exists never waits on them.
func writeCheckpoint(path string, cp checkpoint) error {
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// copyFile is the resume's first move: the checkpoint's books become the
// working database before the ledger is opened over it.
func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
