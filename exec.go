package hisepress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultExecTimeout = 5 * time.Minute
	maxExecOutput      = 10 << 20 // per stream
	execWaitDelay      = 2 * time.Second
)

// Exec runs an external program as a step. The program receives a JSON
// document on stdin and prints a JSON document to stdout, which becomes the
// step's result (so assignments and conditions work as they do for requests).
//
// stdin: {"args": {...}, "vars": {...}, "outDir": "..."}
// "vars" is present only when PassVars is set, since suite variables may hold
// secrets. The same information is available as environment variables:
// HISEPRESS_OUT_DIR and HISEPRESS_ARG_<NAME> for each arg.
//
// A non-zero exit status fails the step; stderr is written to the run log and
// quoted in the error. A script can attach files to the run by including
// "_artifacts": [{"type": "...", "path": "..."}] in its output.
type Exec struct {
	// Command is the program and its arguments. It is run directly, never
	// through a shell; use ["bash", "-c", "..."] if you want one.
	Command []string `json:"command"`
	// Args are passed to the program in the stdin document.
	Args map[string]string `json:"args,omitempty"`
	// Env adds environment variables to the inherited environment.
	Env map[string]string `json:"env,omitempty"`
	// PassVars includes all suite variables in the stdin document.
	PassVars bool `json:"passVars,omitempty"`
	// TimeoutSeconds bounds the run. Default 300.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

func (s *Suite) hasExec() bool {
	for _, p := range s.Auth {
		if p.Type == AuthExec {
			return true
		}
	}
	for _, st := range s.Steps {
		if st.Exec != nil {
			return true
		}
	}
	return false
}

// runExec executes step.Exec and returns its (JSON-decoded) result.
func (r *runner) runExec(ctx context.Context, step *Step) (any, error) {
	e := step.Exec
	if len(e.Command) == 0 || e.Command[0] == "" {
		return nil, fmt.Errorf("exec has no command")
	}
	timeout := defaultExecTimeout
	if e.TimeoutSeconds > 0 {
		timeout = time.Duration(e.TimeoutSeconds) * time.Second
	}
	if r.opts.Untrusted {
		return nil, configErr("exec steps are not allowed in untrusted mode")
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	input := map[string]any{"args": e.Args, "outDir": r.opts.OutDir}
	if e.Args == nil {
		input["args"] = map[string]string{}
	}
	if e.PassVars {
		input["vars"] = r.vars
	}
	stdin, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("cannot encode exec input: %w", err)
	}

	cmd := exec.CommandContext(ctx, e.Command[0], e.Command[1:]...)
	cmd.Dir = r.dir
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "HISEPRESS_OUT_DIR="+r.opts.OutDir)
	for k, v := range e.Args {
		cmd.Env = append(cmd.Env, "HISEPRESS_ARG_"+strings.ToUpper(k)+"="+v)
	}
	for k, v := range e.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	configureProcess(cmd)
	cmd.WaitDelay = execWaitDelay

	r.logf("Running %s\n", strings.Join(e.Command, " "))
	runErr := cmd.Run()
	if stderr.Len() > 0 {
		r.logf("stderr:\n%s\n", strings.TrimRight(stderr.String(), "\n"))
	}
	if parent.Err() != nil {
		return nil, fmt.Errorf("%s was cancelled: %w", e.Command[0], parent.Err())
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s timed out after %s", e.Command[0], timeout)
	}
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return nil, fmt.Errorf("%s exited with status %d. %s",
				e.Command[0], exit.ExitCode(), tail(stderr.String(), 500))
		}
		return nil, fmt.Errorf("could not run %s: %w", e.Command[0], runErr)
	}
	if stdout.over || stderr.over {
		return nil, fmt.Errorf("%s produced more than %d MB of output", e.Command[0], maxExecOutput>>20)
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, nil
	}
	var result any
	if err := json.Unmarshal(out, &result); err != nil {
		// not JSON: keep the text. Assignments will fail with a clear error;
		// a step that only checks the exit status still passes.
		return string(out), nil
	}
	if m, ok := result.(map[string]any); ok {
		if raw, has := m["_artifacts"]; has {
			delete(m, "_artifacts")
			if err := addArtifacts(step, raw); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func addArtifacts(step *Step, raw any) error {
	list, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("_artifacts must be a list of {type, path} objects")
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		path, _ := m["path"].(string)
		if !ok || path == "" {
			return fmt.Errorf("_artifacts entries need a path")
		}
		typ, _ := m["type"].(string)
		step.AddArtifact(typ, path)
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "...." + s[len(s)-n:]
}

// cappedBuffer keeps at most maxExecOutput bytes and records overflow, so a
// runaway program cannot exhaust memory.
type cappedBuffer struct {
	bytes.Buffer
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := maxExecOutput - c.Len(); n > room {
		c.over = true
		c.Buffer.Write(p[:max(room, 0)])
		return n, nil // report success so the child isn't killed by SIGPIPE
	}
	return c.Buffer.Write(p)
}
