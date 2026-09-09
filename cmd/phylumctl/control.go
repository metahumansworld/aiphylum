// Talking to a running phylumd. These four verbs are the whole operator
// surface: put an agent in the world, ask for an episode, watch it happen,
// see where the money ended up. The daemon is on loopback and unauthenticated,
// so there is nothing here but HTTP and JSON.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/metahumansworld/soscitea/internal/daemon"
	"github.com/metahumansworld/soscitea/internal/trace"
)

const defaultAddr = "http://127.0.0.1:8141"

// call makes one request against the daemon and decodes the reply. A non-2xx
// carries the daemon's own explanation, which is the useful half of it — the
// refusals ("no image", "an episode is running", "world is halted") are the
// interface, not incidental errors.
func call(method, addr, path string, body, out any) error {
	var rd *bytes.Reader = bytes.NewReader(nil)
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(addr, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w (is phylumd running?)", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// mountList collects repeated -mount host:container flags.
type mountList []daemon.MountView

func (m *mountList) String() string { return fmt.Sprint(*m) }

func (m *mountList) Set(v string) error {
	host, container, ok := strings.Cut(v, ":")
	if !ok || host == "" || container == "" {
		return fmt.Errorf("want host:container, got %q", v)
	}
	*m = append(*m, daemon.MountView{Host: host, Container: container})
	return nil
}

func submit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	id := fs.String("id", "", "agent id; permanent, and never reused after bankruptcy")
	image := fs.String("image", "", "container image, which must already be present locally")
	grant := fs.Int64("grant", 2500, "opening balance in credits")
	cmdline := fs.String("cmd", "", "command inside the container (default: the image's own)")
	memory := fs.String("memory", "512m", "container memory limit")
	cpus := fs.String("cpus", "1", "container CPU limit")
	var mounts mountList
	fs.Var(&mounts, "mount", "host:container bind mount, read-only; repeatable")
	fs.Parse(args)

	req := daemon.SubmitRequest{
		ID: *id, Image: *image, Grant: *grant,
		Memory: *memory, CPUs: *cpus, Mounts: mounts,
	}
	if *cmdline != "" {
		req.Cmd = strings.Fields(*cmdline)
	}
	var v daemon.AgentView
	if err := call("POST", *addr, "/v1/agents", req, &v); err != nil {
		fail(err)
	}
	fmt.Printf("%s registered — image %s, balance %d\n", v.ID, v.Image, v.Balance)
}

func runEpisode(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	seed := fs.Int64("seed", 1, "episode seed; same seed, same postings")
	rounds := fs.Int("rounds", 4, "rounds in the episode")
	wall := fs.Int("wall", 60, "per-attempt wall-clock ceiling in seconds")
	ceiling := fs.Int64("ceiling", 0, "per-attempt token budget in credits (0 = the world's default)")
	detach := fs.Bool("detach", false, "return as soon as the episode starts instead of waiting for it")
	fs.Parse(args)

	var ep daemon.EpisodeView
	err := call("POST", *addr, "/v1/episodes", daemon.EpisodeRequest{
		Seed: *seed, Rounds: *rounds, WallClockSec: *wall, TokenCeiling: *ceiling,
	}, &ep)
	if err != nil {
		fail(err)
	}
	fmt.Printf("%s started — seed %d, %d rounds, %d postings\n", ep.ID, ep.Seed, ep.Rounds, ep.Postings)
	if *detach {
		return
	}

	// Wait it out. The daemon runs one episode at a time, so polling its
	// record is the whole story; `phylumctl tail` is for watching the events.
	for {
		time.Sleep(500 * time.Millisecond)
		var cur daemon.EpisodeView
		if err := call("GET", *addr, "/v1/episodes/"+ep.ID, nil, &cur); err != nil {
			fail(err)
		}
		if cur.State == "running" {
			continue
		}
		if cur.State == "failed" {
			fmt.Fprintf(os.Stderr, "%s failed: %s\n", cur.ID, cur.Error)
			printRoster(*addr)
			os.Exit(1)
		}
		fmt.Printf("%s done\n", cur.ID)
		printRoster(*addr)
		return
	}
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	fs.Parse(args)

	var st daemon.StatusView
	if err := call("GET", *addr, "/v1/status", nil, &st); err != nil {
		fail(err)
	}
	fmt.Printf("state: %s", st.State)
	if st.Error != "" {
		fmt.Printf(" — %s", st.Error)
	}
	fmt.Println()
	if st.Conservation != "" {
		fmt.Printf("conservation: %s\n", st.Conservation)
	}

	var eps []daemon.EpisodeView
	if err := call("GET", *addr, "/v1/episodes", nil, &eps); err != nil {
		fail(err)
	}
	if len(eps) > 0 {
		fmt.Printf("\n%-6s %-8s %6s %8s %-8s %s\n", "id", "seed", "rounds", "postings", "state", "started")
		for _, e := range eps {
			fmt.Printf("%-6s %-8d %6d %8d %-8s %s\n", e.ID, e.Seed, e.Rounds, e.Postings, e.State, e.Started)
		}
	}
	printRoster(*addr)
}

func printRoster(addr string) {
	var agents []daemon.AgentView
	if err := call("GET", addr, "/v1/agents", nil, &agents); err != nil {
		fail(err)
	}
	if len(agents) == 0 {
		fmt.Println("\nno agents")
		return
	}
	fmt.Printf("\n%-12s %-28s %8s %8s  %s\n", "agent", "image", "grant", "balance", "")
	for _, a := range agents {
		fate := ""
		if a.Retired {
			fate = "bankrupt"
		}
		fmt.Printf("%-12s %-28s %8d %8d  %s\n", a.ID, a.Image, a.Grant, a.Balance, fate)
	}
}

// tail streams the live trace, printing the same one-line summaries as
// `phylumctl trace` — the difference is that this one never reaches the end.
func tail(args []string) {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	fs.Parse(args)

	resp, err := http.Get(strings.TrimSuffix(*addr, "/") + "/v1/trace?follow=1")
	if err != nil {
		fail(fmt.Errorf("%s: %w (is phylumd running?)", *addr, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		fail(fmt.Errorf("trace: %s", resp.Status))
	}

	sc := bufio.NewScanner(resp.Body)
	// Trace lines carry whole model-call payloads; the default 64KB token is
	// not always enough.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var l trace.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue // a partially flushed line; the next read will have it whole
		}
		fmt.Printf("%4d %-10s %s\n", l.Seq, l.Type, trace.Summary(l))
	}
	if err := sc.Err(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
	os.Exit(1)
}

// join puts a guest into a fair that is already running: the path goes to
// phylumd's door, phylumd seats the body on its next tick, and the answer
// names the minute it landed in. Absolute so the daemon's working directory
// is not the client's problem; everything else about the file is checked on
// the far side, where it will be exec'd.
func join(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	ranked := fs.Bool("ranked", false, "enrol the guest for ranked work: it sits the fair's daily sitting wherever it stands, and what it wins there goes on the ladder")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	path, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
		os.Exit(1)
	}
	var out struct {
		ID     string `json:"id"`
		Day    int    `json:"day"`
		Clock  string `json:"clock"`
		Ranked bool   `json:"ranked"`
	}
	body := map[string]any{"path": path}
	if *ranked {
		body["ranked"] = true
	}
	if err := call("POST", *addr, "/v1/guests", body, &out); err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s joined the fair on day %d at %s", out.ID, out.Day, out.Clock)
	if out.Ranked {
		fmt.Printf(", enrolled for the sitting")
	}
	fmt.Println()
}

// leave takes a guest out of a running fair by name. The daemon decides
// whether the name is a guest's and what it leaves with; the client only
// carries the name and repeats the answer.
func leave(args []string) {
	fs := flag.NewFlagSet("leave", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "phylumd control address")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
		os.Exit(2)
	}
	var out struct {
		ID      string `json:"id"`
		Day     int    `json:"day"`
		Clock   string `json:"clock"`
		Balance int64  `json:"balance"`
	}
	if err := call("DELETE", *addr, "/v1/guests/"+url.PathEscape(fs.Arg(0)), nil, &out); err != nil {
		fmt.Fprintf(os.Stderr, "phylumctl: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s left the fair on day %d at %s with %d credits\n", out.ID, out.Day, out.Clock, out.Balance)
}
