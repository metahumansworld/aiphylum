// dungeonctl inspects the platform from the outside. For now that means
// traces — the public record of everything an episode did.
//
//	dungeonctl trace <file>          every event, one line each
//	dungeonctl calls <file>          just the metered model calls, with a total
//	dungeonctl serve <file> [addr]   browse the episode: leaderboard, agent and
//	                                 bounty pages, replay viewer
//
// TODO(daemon): submit/run/tail against a running dungeond once it has an
// episode intake API.
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/metahunmei/dungeon/internal/trace"
	"github.com/metahunmei/dungeon/web"
)

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	cmd, path := os.Args[1], os.Args[2]

	lines, err := trace.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "trace":
		for i, l := range lines {
			fmt.Printf("%4d %-10s %s\n", i, l.Type, trace.Summary(l))
		}
	case "calls":
		printCalls(lines)
	case "serve":
		addr := "127.0.0.1:8140"
		if len(os.Args) > 3 {
			addr = os.Args[3]
		}
		srv, err := web.NewServer(path, lines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("serving %s on http://%s\n", path, addr)
		if err := http.ListenAndServe(addr, srv); err != nil {
			fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dungeonctl trace|calls|serve <trace-file> [addr]")
}

func printCalls(lines []trace.Line) {
	calls, err := trace.ModelCalls(lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%-4s %-24s %-10s %6s %6s %8s  %s\n",
		"#", "wallet", "model", "in", "out", "cost", "outcome")
	var total int64
	for i, c := range calls {
		status := string(c.Outcome)
		if c.Error != "" {
			status += ": " + c.Error
		}
		fmt.Printf("%-4d %-24s %-10s %6d %6d %8d  %s\n",
			i, c.Wallet, c.Model, c.Usage.InputTokens, c.Usage.OutputTokens, c.Cost, status)
		total += int64(c.Cost)
	}
	fmt.Printf("%d calls, %d credits metered\n", len(calls), total)
}
