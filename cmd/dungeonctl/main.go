// dungeonctl inspects the platform from the outside. For now that means
// traces — the public record of everything an episode did.
//
//	dungeonctl trace <file>          every event, one line each
//	dungeonctl calls <file>          just the metered model calls, with a total
//	dungeonctl serve [-follow] <file> [addr]
//	                                 browse the episode: leaderboard, agent and
//	                                 bounty pages, replay viewer. -follow tails
//	                                 a trace still being written and streams it
//	                                 to the browser live.
//
// TODO(daemon): submit/run/tail against a running dungeond once it has an
// episode intake API.
package main

import (
	"flag"
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

	switch os.Args[1] {
	case "trace":
		for i, l := range readTrace(os.Args[2]) {
			fmt.Printf("%4d %-10s %s\n", i, l.Type, trace.Summary(l))
		}
	case "calls":
		printCalls(readTrace(os.Args[2]))
	case "serve":
		serve(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dungeonctl trace|calls <trace-file> | serve [-follow] <trace-file> [addr]")
}

func readTrace(path string) []trace.Line {
	lines, err := trace.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
		os.Exit(1)
	}
	return lines
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	follow := fs.Bool("follow", false, "tail a trace still being written; pages and the replay viewer update live")
	fs.Parse(args)
	if fs.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	addr := "127.0.0.1:8140"
	if fs.NArg() > 1 {
		addr = fs.Arg(1)
	}

	var srv *web.Server
	var err error
	if *follow {
		srv, err = web.NewLiveServer(path)
	} else {
		srv, err = web.NewServer(path, readTrace(path))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
		os.Exit(1)
	}
	mode := ""
	if *follow {
		mode = " (live)"
	}
	fmt.Printf("serving %s%s on http://%s\n", path, mode, addr)
	if err := http.ListenAndServe(addr, srv); err != nil {
		fmt.Fprintf(os.Stderr, "dungeonctl: %v\n", err)
		os.Exit(1)
	}
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
