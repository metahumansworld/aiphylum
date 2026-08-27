package trace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTailConsumesOnlyCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	write := func(s string) {
		t.Helper()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing yet.
	lines, off, err := Tail(path, 0)
	if err != nil || len(lines) != 0 || off != 0 {
		t.Fatalf("empty file: lines=%d off=%d err=%v", len(lines), off, err)
	}

	// One complete line and one torn one: only the complete line comes back,
	// and the offset stops at its newline so the torn bytes are re-read later.
	write(`{"seq":1,"type":"note","payload":{"note":"a"}}` + "\n")
	write(`{"seq":2,"type":"no`)
	lines, off, err = Tail(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Seq != 1 {
		t.Fatalf("torn tail: got %d lines, want just seq 1", len(lines))
	}

	// Completing the torn line makes it visible from the saved offset.
	write(`te","payload":{"note":"b"}}` + "\n")
	lines, off2, err := Tail(path, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Seq != 2 {
		t.Fatalf("after completion: got %+v, want just seq 2", lines)
	}

	// Fully consumed: nothing more, offset stable.
	lines, off3, err := Tail(path, off2)
	if err != nil || len(lines) != 0 || off3 != off2 {
		t.Fatalf("at tip: lines=%d off=%d→%d err=%v", len(lines), off2, off3, err)
	}

	// The offsets must partition the file exactly.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if off3 != fi.Size() {
		t.Errorf("final offset %d, want file size %d", off3, fi.Size())
	}
}

func TestTailReportsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Tail(path, 0); err == nil {
		t.Fatal("corrupt line silently accepted")
	}
}
