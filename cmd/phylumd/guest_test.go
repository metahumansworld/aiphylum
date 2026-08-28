package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGuestRoster pins the intake rules: the filename becomes the name, the
// name gets the cast's alphabet, and no name is handed out twice — because an
// id reaching two owners would merge their wallets and their walkers.
func TestGuestRoster(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("# a guest\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("filename becomes the name", func(t *testing.T) {
		gs, err := guestRoster([]string{mk("pilgrim.py")}, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(gs) != 1 || gs[0].id != "pilgrim" {
			t.Fatalf("got %+v", gs)
		}
		if !filepath.IsAbs(gs[0].path) {
			t.Errorf("path %q is not absolute", gs[0].path)
		}
	})

	t.Run("case and underscores normalize", func(t *testing.T) {
		gs, err := guestRoster([]string{mk("My_Trader.py")}, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if gs[0].id != "my-trader" {
			t.Fatalf("got id %q", gs[0].id)
		}
	})

	t.Run("only python crosses the door", func(t *testing.T) {
		if _, err := guestRoster([]string{mk("trader.go")}, map[string]bool{}); err == nil {
			t.Error("a .go file was admitted")
		}
	})

	t.Run("a name the world gave out is refused", func(t *testing.T) {
		_, err := guestRoster([]string{mk("frugal.py")}, map[string]bool{"frugal": true})
		if err == nil || !strings.Contains(err.Error(), "taken") {
			t.Errorf("collision with the cast not refused: %v", err)
		}
	})

	// Roster order is the order the -guest flags were written, and it survives
	// all the way to the auction: the fair walks the town's standings in roster
	// order, so the first flag is shown each board first and takes the earlier
	// arrival — which is how the auction breaks a tie. `make fair-rivals` runs
	// two copies of one strategy and the first flag wins every tie between
	// them, so a sort quietly introduced here would not fail anything loudly.
	// It would just hand the win to whichever name came first in the alphabet.
	t.Run("the flags keep the order they were written in", func(t *testing.T) {
		gs, err := guestRoster([]string{mk("zeta.py"), mk("alpha.py")}, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(gs) != 2 || gs[0].id != "zeta" || gs[1].id != "alpha" {
			t.Fatalf("roster is not in flag order: %+v", gs)
		}
	})

	t.Run("two guests, one filename, refused", func(t *testing.T) {
		p := mk("twin.py")
		if _, err := guestRoster([]string{p, p}, map[string]bool{}); err == nil {
			t.Error("duplicate guest admitted")
		}
	})

	t.Run("a name outside the alphabet is refused", func(t *testing.T) {
		for _, bad := range []string{"3rd.py", "a.py", "much-too-long-a-name-for-anyone.py"} {
			if _, err := guestRoster([]string{mk(bad)}, map[string]bool{}); err == nil {
				t.Errorf("%s was admitted", bad)
			}
		}
	})

	// The ledger's own accounts are namespaced sys: — mint, burn, provider,
	// hold — and minting is the one operation that makes credits from nothing.
	// The alphabet has no colon in it, so those names are unreachable rather
	// than merely unlisted; this is the test that keeps it that way. The
	// filenames need not exist: the name is judged before the file is looked
	// for, which is the order that makes this checkable at all.
	t.Run("the ledger's system accounts are out of reach", func(t *testing.T) {
		for _, sys := range []string{"sys:mint.py", "sys:burn.py", "sys:provider.py", "sys:hold.py"} {
			bad := filepath.Join(dir, sys)
			if _, err := guestRoster([]string{bad}, map[string]bool{}); err == nil {
				t.Errorf("%s was admitted — a guest reached a system account", sys)
			}
		}
	})

	t.Run("a file that is not there is refused", func(t *testing.T) {
		if _, err := guestRoster([]string{filepath.Join(dir, "ghost.py")}, map[string]bool{}); err == nil {
			t.Error("a missing file was admitted")
		}
	})
}
