package main

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// precheckTimeout caps how long a schedule's precheck command may run, so a
// hung script cannot wedge the scheduler goroutine or a headless run.
const precheckTimeout = 60 * time.Second

// runPrecheck runs a schedule's precheck command (a shell string) in dir.
// It returns:
//
//	(true, nil)   command exited 0       → something changed, run the prompt
//	(false, nil)  command exited non-0   → nothing new, skip (no tokens)
//	(false, err)  command could not run  → record an error, do not run
//
// The middle case (a clean non-zero exit) is the normal "no change" signal
// and is not an error. The last case (missing script, bad dir, timeout)
// is surfaced so a broken precheck shows up in the run log instead of
// silently suppressing every fire.
func runPrecheck(cmd, dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), precheckTimeout)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/c", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	c.Dir = dir

	err := c.Run()
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil // ran to completion, non-zero status = "no change"
	}
	return false, err // failed to start / timed out
}
