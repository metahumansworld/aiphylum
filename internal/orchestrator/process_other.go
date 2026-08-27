//go:build !unix

package orchestrator

import "os/exec"

// isolate does nothing off Unix: there is no process group to signal, so a step
// that leaves a child holding stdout is bounded by WaitDelay alone. Nothing in
// this project runs there today — the sandbox shells out to docker and the
// process track to /bin/sh — but a platform we do not support should fail to
// run, not fail to build.
func isolate(*exec.Cmd) {}
