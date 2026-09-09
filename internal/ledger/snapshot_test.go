package ledger

import (
	"context"
	"path/filepath"
	"testing"
)

// A snapshot is the books as they stood when it was taken: what is posted
// after it is not in it, and what was posted before it is.
func TestSnapshotIsTheBooksAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.CreateAccount(ctx, "a", KindAgent, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Mint(ctx, "a", 100, "grant", "grant:a"); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "boundary.db")
	if err := l.Snapshot(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Burn(ctx, "a", 40, "after", "after:a"); err != nil {
		t.Fatal(err)
	}
	c, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bal, err := c.Balance(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 100 {
		t.Fatalf("snapshot balance = %d, want 100 (the burn came after)", bal)
	}
	if err := c.Verify(ctx); err != nil {
		t.Fatalf("snapshot does not audit: %v", err)
	}
}
