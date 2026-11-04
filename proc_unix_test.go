//go:build !windows

package hisepress

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecTimeoutKillsChildProcesses(t *testing.T) {
	needPython(t)
	pidfile := t.TempDir() + "/pid"
	s := execSuite(t, `
steps:
  - exec:
      command: [python3, spawner.py]
      args: {pidfile: `+pidfile+`}
      timeoutSeconds: 1
`)
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "timed out") {
		t.Fatalf("expected a timeout, got %+v", res.Steps[0])
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the script never recorded its child's pid: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // gone
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL) // don't leave it behind
			t.Fatalf("child process %d survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
