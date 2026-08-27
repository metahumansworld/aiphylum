package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Tail parses the complete lines of a trace file from byte offset off onward.
// The writer may be mid-append: the flush is per line but a line larger than
// the write buffer can land in pieces, so only bytes up to the last newline
// are consumed and a torn tail is left for the next call. Returns the parsed
// lines and the offset just past the last complete line, from which the next
// call resumes. On error the original offset comes back, so a retry re-reads
// the same region.
func Tail(path string, off int64) ([]Line, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, fmt.Errorf("seek trace: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, off, fmt.Errorf("read trace: %w", err)
	}
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 {
		return nil, off, nil
	}

	var lines []Line
	end := off
	for _, raw := range bytes.Split(data[:cut], []byte{'\n'}) {
		end += int64(len(raw)) + 1
		if len(raw) == 0 {
			continue
		}
		var l Line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, off, fmt.Errorf("trace line at offset %d: %w", end-int64(len(raw))-1, err)
		}
		lines = append(lines, l)
	}
	return lines, end, nil
}
