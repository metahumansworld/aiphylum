// Package generators adapts the Python task generators in generators/ to the
// board's Generator interface. The plan keeps task supply in Python — it is
// the native language of the reference solvers and of anyone adding tasks —
// so the Go side is a thin shell-out: run the script with --seed and --tier,
// read one Task JSON line back.
package generators

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/metahunmei/dungeon/internal/bounty"
)

// Script is one Python generator file exposed as a bounty.Generator.
type Script struct {
	GenName string // the name bounties are posted under
	Path    string // path to the .py file
	Python  string // interpreter; empty means "python3"
}

func (s Script) Name() string { return s.GenName }

func (s Script) Generate(seed int64, tier int) (bounty.Task, error) {
	python := s.Python
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, s.Path,
		"--seed", fmt.Sprint(seed), "--tier", fmt.Sprint(tier))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return bounty.Task{}, fmt.Errorf("generator %s (seed %d, tier %d): %w: %s",
			s.GenName, seed, tier, err, stderr.String())
	}

	var task bounty.Task
	if err := json.Unmarshal(stdout.Bytes(), &task); err != nil {
		return bounty.Task{}, fmt.Errorf("generator %s: bad task JSON: %w", s.GenName, err)
	}
	// A task needs a prompt, a price, and exactly one hidden key: an answer to
	// verify against, or a rubric to be judged against. Both or neither is a
	// broken generator, not an open-ended task.
	if task.Prompt == "" || !task.Keyed() || task.ReferenceTokens <= 0 {
		return bounty.Task{}, fmt.Errorf("generator %s: incomplete task: %+v", s.GenName, task)
	}
	// Suite provenance is the loader's to assert, not a script's to claim. A
	// generated task that labelled itself imported would wear an asterisk it
	// hadn't earned; one that claimed a real suite's name would launder a
	// generated instance into that suite's record.
	if task.Suite != "" {
		return bounty.Task{}, fmt.Errorf(
			"generator %s: claimed suite %q — suite provenance comes from an imported suite file, not from a generator",
			s.GenName, task.Suite)
	}
	return task, nil
}

// Dir returns the demo generators from a generators/ directory, ready to
// register with a board.
func Dir(dir string) []Script {
	return []Script{
		{GenName: "arith", Path: filepath.Join(dir, "arith.py")},
		{GenName: "oracle", Path: filepath.Join(dir, "oracle.py")},
		// brief is judged, not keyed. Registering it here is safe in either
		// world: a ranked orchestrator refuses to post one, so the ladder
		// cannot be reached by a bounty a model decided.
		{GenName: "brief", Path: filepath.Join(dir, "brief.py")},
	}
}
