package main

import (
	"runtime"
	"testing"
)

func TestRunPrecheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell semantics differ on windows")
	}
	dir := t.TempDir()

	// Exit 0 → run the prompt.
	if run, err := runPrecheck("exit 0", dir); err != nil || !run {
		t.Errorf("exit 0: run=%v err=%v, want run=true err=nil", run, err)
	}

	// Clean non-zero exit → skip, and it is NOT an error (the normal
	// "nothing changed" signal).
	if run, err := runPrecheck("exit 3", dir); err != nil || run {
		t.Errorf("exit 3: run=%v err=%v, want run=false err=nil", run, err)
	}

	// Command that cannot run at all (working dir does not exist) → error,
	// so a broken precheck surfaces instead of silently skipping forever.
	if run, err := runPrecheck("exit 0", "/no/such/dir/atc-precheck"); err == nil || run {
		t.Errorf("bad dir: run=%v err=%v, want run=false err!=nil", run, err)
	}
}
