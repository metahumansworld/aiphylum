// phylumctl drives the platform from the outside — either a trace file it
// already produced, or a phylumd running live.
//
// Over a trace file:
//
//	phylumctl trace <file>          every event, one line each
//	phylumctl calls <file>          just the metered model calls, with a total
//	phylumctl serve [-follow] <file> [addr]
//	                                 browse the episode: leaderboard, agent and
//	                                 bounty pages, replay viewer. -follow tails
//	                                 a trace still being written and streams it
//	                                 to the browser live.
//
// Against a running phylumd (-addr, default http://127.0.0.1:8141):
//
//	phylumctl submit -id X -image Y [-grant N] [-cmd ...] [-mount h:c]
//	                                 put an agent in the world with a wallet
//	phylumctl run [-seed N] [-rounds N]
//	                                 ask for an episode and wait for it
//	phylumctl tail                  watch the live trace as it is written
//	phylumctl status                world state, episodes, and the roster
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/metahumansworld/aiphylum/internal/trace"
	"github.com/metahumansworld/aiphylum/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "trace", "calls":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if os.Args[1] == "calls" {
			printCalls(readTrace(os.Args[2]))
			return
		}
		for i, l := range readTrace(os.Args[2]) {
			fmt.Printf("%4d %-10s %s\n", i, l.Type, trace.Summary(l))
		}
	case "serve":
		serve(os.Args[2:])
	case "submit":
		submit(os.Args[2:])
	case "run":
		runEpisode(os.Args[2:])
	case "tail":
		tail(os.Args[2:])
	case "status":
		status(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  phylumctl trace|calls <trace-file>")
	fmt.Fprintln(os.Stderr, "  phylumctl serve [-follow] <trace-file> [addr]")
	fmt.Fprintln(os.Stderr, "  phylumctl submit -id <id> -image <image> [-grant n] [-cmd ...] [-mount host:container]")
	fmt.Fprintln(os.Stderr, "  phylumctl run [-seed n] [-rounds n] [-detach]")
	fmt.Fprintln(os.Stderr, "  phylumctl tail")
	fmt.Fprintln(os.Stderr, "  phylumctl status")
	fmt.Fprintln(os.Stderr, "\nthe last four take -addr (default "+defaultAddr+") and talk to a running phylumd")
}

func readTrace(path string) []trace.Line {
	lines, err := trace.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
		os.Exit(1)
	}
	mode := ""
	if *follow {
		mode = " (live)"
	}
	fmt.Printf("serving %s%s on http://%s\n", path, mode, addr)
	if err := http.ListenAndServe(addr, srv); err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
		os.Exit(1)
	}
}

func printCalls(lines []trace.Line) {
	calls, err := trace.ModelCalls(lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
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
