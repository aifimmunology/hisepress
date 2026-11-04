package hisepress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PaesslerAG/jsonpath"
)

// Options configures a run. The zero value is usable.
type Options struct {
	// Output receives the human-readable run log. Nil discards it.
	Output io.Writer
	// Logger receives diagnostics. Nil uses slog.Default().
	Logger *slog.Logger
	// HTTPClient is used for requests. Nil uses a client with a 60s timeout.
	HTTPClient *http.Client
	// Auth, if set, decorates every request.
	Auth Authenticator
	// Funcs are code functions available to this run, in addition to those
	// added with Register. These take precedence.
	Funcs map[string]Func
	// Overrides replace suite variables by name (matched ignoring case). The
	// replacement is converted to the type of the variable it replaces.
	Overrides map[string]string
	// Verbose logs complete responses rather than a short preview.
	Verbose bool
	// Untrusted restricts what the suite may do; see untrusted.go. Run returns
	// an *UntrustedError, before executing anything, if the suite breaks the
	// rules that can be checked up front.
	Untrusted bool
	// AllowEnv lists the environment variables auth profiles may read in
	// untrusted mode.
	AllowEnv []string
	// OutDir is where steps should write files they produce (see
	// Step.OutDir). Run does not create it; the caller does.
	OutDir string
	// RetryInitialDelay is the first wait between retries of a step with
	// wait set. It doubles each time, up to one minute. Default one second.
	RetryInitialDelay time.Duration
}

// StepResult is the outcome of one step.
type StepResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// Result is the outcome of a suite run.
type Result struct {
	Suite      string       `json:"suite"`
	File       string       `json:"file,omitempty"`
	Status     string       `json:"status"`
	Start      time.Time    `json:"start"`
	DurationMS int64        `json:"durationMs"`
	Steps      []StepResult `json:"steps"`
	Artifacts  []Artifact   `json:"artifacts,omitempty"`

	// Log is the full human-readable run log.
	Log string `json:"-"`
}

// Passed reports whether the suite passed.
func (r *Result) Passed() bool { return r.Status == StatusPass }

type runner struct {
	opts      Options
	log       *slog.Logger
	out       io.Writer
	client    *http.Client
	vars      map[string]any
	artifacts []Artifact
	dir       string // working directory for exec steps
	profiles  map[string]AuthProfile
	defAuth   string
	auths     map[string]Authenticator // built stateful authenticators

	secretVars []string            // suite.secrets
	secrets    map[string]struct{} // fixed secret strings to redact
	live       []secretProvider    // authenticators whose secrets change
}

func (r *runner) logf(format string, args ...any) {
	fmt.Fprint(r.out, r.redact(fmt.Sprintf(format, args...)))
}

const (
	redacted      = "[REDACTED]"
	minAutoSecret = 8 // shorter auth secrets are only redacted via suite.secrets
)

// redact removes known secrets from s: values held by auth profiles (and the
// header values built from them), and the values of variables the suite lists
// under secrets. It is best-effort: it matches exact strings, so a secret that
// a server transforms before echoing it back is not caught.
func (r *runner) redact(s string) string {
	var list []string
	for v := range r.secrets {
		list = append(list, v)
	}
	for _, p := range r.live {
		for _, v := range p.Secrets() {
			if len(v) >= minAutoSecret {
				list = append(list, v)
			}
		}
	}
	for _, name := range r.secretVars {
		if v, ok := r.vars[name]; ok {
			if str := stringify(v); str != "" {
				list = append(list, str)
			}
		}
	}
	// longest first, so a secret containing another is replaced whole
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, v := range list {
		s = strings.ReplaceAll(s, v, redacted)
	}
	return s
}

func (r *runner) trackAuth(a Authenticator, stateful bool) {
	sp, ok := a.(secretProvider)
	if !ok {
		return
	}
	if stateful {
		r.live = append(r.live, sp)
		return
	}
	for _, v := range sp.Secrets() {
		if len(v) >= minAutoSecret {
			r.secrets[v] = struct{}{}
		}
	}
}

// Run executes the suite. An error is returned only when the run could not
// start (bad variable overrides, unresolvable variables); test failures are
// reported in the Result. Run does not modify the Suite.
func (s *Suite) Run(ctx context.Context, opts Options) (*Result, error) {
	r := &runner{opts: opts, log: opts.Logger, client: opts.HTTPClient, secrets: map[string]struct{}{}, secretVars: s.Secrets}
	var logBuf bytes.Buffer
	r.out = &logBuf
	if opts.Output != nil {
		r.out = io.MultiWriter(opts.Output, &logBuf)
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if opts.Untrusted {
		if reasons := s.untrustedViolations(opts.AllowEnv); len(reasons) > 0 {
			sort.Strings(reasons)
			return nil, &UntrustedError{Reasons: reasons}
		}
		if r.client == nil {
			r.client = untrustedClient()
		} else {
			r.log.Warn("untrusted mode cannot block link-local addresses when a custom HTTPClient is supplied")
		}
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: 60 * time.Second}
	}
	r.profiles, r.defAuth, r.auths = s.Auth, s.DefaultAuth, map[string]Authenticator{}
	if a := opts.Auth; a != nil {
		r.trackAuth(a, true)
	}
	if s.File != "" {
		r.dir = filepath.Dir(s.File)
	}
	r.vars = map[string]any{}
	for k, v := range s.Variables {
		r.vars[k] = v
	}
	if err := r.applyOverrides(opts.Overrides); err != nil {
		return nil, err
	}
	if err := resolveVariables(r.vars); err != nil {
		return nil, err
	}

	res := &Result{Suite: s.Name, File: s.File, Start: time.Now()}
	status := StatusPass
	r.logf("Beginning run of suite %q. Steps: %d\n", s.Name, len(s.Steps))

	for i, step := range s.Steps {
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("Step %d", i+1)
		}
		sr := StepResult{Name: name}
		begin := time.Now()
		record := func() {
			sr.DurationMS = time.Since(begin).Milliseconds()
			res.Steps = append(res.Steps, sr)
		}

		if ctx.Err() != nil && status == StatusPass {
			r.logf("***Run cancelled: %s***\n", ctx.Err())
			status = StatusError
		}
		if status != StatusPass && !step.AlwaysRun {
			r.logf("==Skipping step %s (SUITE FAILED)==\n", name)
			sr.Status = StatusSkipped
			record()
			continue
		}
		if skip, serr := r.shouldSkip(step, name); serr != nil {
			if status == StatusPass {
				status = StatusError
			}
			sr.Status, sr.Error = StatusError, r.redact(serr.Error())
			record()
			continue
		} else if skip {
			sr.Status = StatusSkipped
			record()
			continue
		}
		if status != StatusPass {
			r.logf("==Executing step %s (FORCED)==\n", name)
		} else {
			r.logf("==Executing step %s==\n", name)
		}
		if step.Description != "" {
			desc, err := expand(step.Description, func(n string) (string, bool) {
				v, ok := r.vars[n]
				return stringify(v), ok
			}, nil)
			if err != nil {
				desc = step.Description
			}
			r.logf("-%s-\n", desc)
		}

		r.log.Debug("running step", "suite", s.Name, "step", name)
		stepCtx := ctx
		if step.AlwaysRun && ctx.Err() != nil {
			// cleanup must still be able to run after Ctrl-C
			stepCtx = context.WithoutCancel(ctx)
		}
		if err := r.execute(stepCtx, step); err != nil {
			sr.Status, sr.Error = StatusFail, r.redact(err.Error())
			if step.AlwaysContinue {
				r.logf("==Step failed, but alwaysContinue is set; continuing==\n")
			} else {
				r.logf("***Step failed. SUITE FAILED.***\n")
				status = StatusFail
			}
			r.logf("\tError: %s\n", err)
		} else {
			sr.Status = StatusPass
			r.logf("==Step %s completed successfully==\n", name)
		}
		record()
	}

	r.logf("Final Status: %s\n", status)
	res.Status = status
	res.Artifacts = r.artifacts
	res.Log = logBuf.String()
	res.DurationMS = time.Since(res.Start).Milliseconds()
	return res, nil
}

func (r *runner) shouldSkip(step Step, name string) (bool, error) {
	for _, c := range step.SkipConditions {
		got, err := evaluate(c.Condition, r.vars)
		if err != nil {
			r.logf("***Skip condition %s in step %s failed to evaluate. SUITE ERROR.***\n\t%s\n",
				c.Condition, name, err)
			return false, fmt.Errorf("skip condition %q failed to evaluate: %w", c.Condition, err)
		}
		if matches(c.ExpectedValue, got) {
			r.logf("==Step %s skip condition %s evaluated to %v. Skipping step==\n",
				name, c.Condition, c.ExpectedValue)
			return true, nil
		}
	}
	return false, nil
}

// applyOverrides replaces suite variables, keeping the original type.
func (r *runner) applyOverrides(overrides map[string]string) error {
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, found := "", false
		for existing := range r.vars {
			if strings.EqualFold(existing, k) {
				name, found = existing, true
				break
			}
		}
		if !found {
			return fmt.Errorf("cannot override variable %q: the suite does not define it", k)
		}
		nv, err := convertLike(r.vars[name], overrides[k])
		if err != nil {
			return fmt.Errorf("override for variable %q: %w", name, err)
		}
		r.vars[name] = nv
	}
	return nil
}

func convertLike(existing any, s string) (any, error) {
	switch existing.(type) {
	case bool:
		return strconv.ParseBool(s)
	case float64:
		return strconv.ParseFloat(s, 64)
	case int:
		return strconv.Atoi(s)
	case string, nil:
		return s, nil
	default:
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("expected JSON for a %T variable: %w", existing, err)
		}
		return v, nil
	}
}

func toGeneric(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *runner) execute(ctx context.Context, orig Step) error {
	step, err := substituteStep(orig, r.vars)
	if err != nil {
		return fmt.Errorf("variable substitution failed: %w", err)
	}
	step.outDir = r.opts.OutDir
	defer func() { r.artifacts = append(r.artifacts, step.artifacts...) }()

	if step.Code != nil || step.Exec != nil {
		var raw any
		var err error
		name := ""
		if step.Code != nil {
			name = step.Code.Name
			fn, ok := lookupFunc(name, r.opts.Funcs)
			if !ok {
				return fmt.Errorf("no function named %q has been registered", name)
			}
			raw, err = fn(ctx, &step, step.Code.Args)
		} else {
			name = "exec"
			raw, err = r.runExec(ctx, &step)
		}
		if err != nil {
			return err
		}
		result, err := toGeneric(raw)
		if err != nil {
			return fmt.Errorf("%s returned a result that cannot be represented as JSON: %w", name, err)
		}
		if err := r.assign(step, result); err != nil {
			return err
		}
	}

	timeout := time.Hour
	if step.TimeoutInMinutes > 0 {
		timeout = time.Duration(step.TimeoutInMinutes) * time.Minute
	}
	deadline := time.Now().Add(timeout)
	sleep := r.opts.RetryInitialDelay
	if sleep <= 0 {
		sleep = time.Second
	}
	const maxSleep = time.Minute

	for {
		stepErr, summary := r.attempt(ctx, &step)
		if stepErr == nil {
			r.logf("%s", summary)
			return nil
		}
		if !step.Wait || isConfigError(stepErr) {
			r.logf("%s", summary)
			return stepErr
		}
		if time.Now().After(deadline) {
			r.logf("%s", summary)
			return fmt.Errorf("step timed out after %s. Last error was: %w", timeout, stepErr)
		}
		if err := r.checkFailureConditions(step); err != nil {
			r.logf("%s", summary)
			return err
		}
		r.logf(" Step has error. %s.\n Wait is set, will retry in %s\n", stepErr, sleep)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
		if sleep < maxSleep {
			sleep *= 2
		}
	}
}

// attempt makes one try at a step. summary is what to write to the run log.
func (r *runner) attempt(ctx context.Context, step *Step) (stepErr error, summary string) {
	if step.Request != nil {
		r.logf("Doing request %s %s\n", orDefault(step.Request.Method, "GET"), step.Request.URL)
		if err := r.resolveFiles(step.Request); err != nil {
			return err, ""
		}
		auth, err := r.authFor(step.Auth)
		if err != nil {
			return err, ""
		}
		resp, err := step.Request.Do(ctx, r.client, auth)
		if err != nil {
			return err, ""
		}
		if err := step.checkStatus(resp); err != nil {
			return err, ""
		}
		var result any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			result = nil
		}
		summary = r.preview(resp.Body, result)
		if step.ExpectedResultsLength != nil {
			arr, ok := result.([]any)
			if !ok {
				return fmt.Errorf("expectedResultsLength is set, but the response is not a JSON array"), summary
			}
			if *step.ExpectedResultsLength != len(arr) {
				return fmt.Errorf("expected results length was %d, but actual results length was %d",
					*step.ExpectedResultsLength, len(arr)), summary
			}
		}
		if result != nil {
			if err := r.assign(*step, result); err != nil {
				return err, summary
			}
		} else if len(step.Assignments) > 0 {
			return fmt.Errorf("response was not JSON, so assignments cannot be made"), summary
		}
	}
	for _, c := range step.Conditions {
		r.logf("Evaluating step condition '%s'. Expected value: %v\n", c.Condition, c.ExpectedValue)
		got, err := evaluate(c.Condition, r.vars)
		if err != nil {
			return fmt.Errorf("condition %s failed to evaluate. %w", c.Condition, err), summary
		}
		if !matches(c.ExpectedValue, got) {
			return fmt.Errorf("condition %s failed with value %v instead of expected value %v",
				c.Condition, got, c.ExpectedValue), summary
		}
		r.logf(" Step condition succeeded\n")
	}
	return nil, summary
}

// authFor picks the authenticator for a step: its own profile, else the
// suite default, else Options.Auth. "none" disables authentication.
func (r *runner) authFor(name string) (Authenticator, error) {
	if name == "" {
		name = r.defAuth
	}
	switch name {
	case authNone:
		return nil, nil
	case "":
		return r.opts.Auth, nil
	}
	if a, ok := r.auths[name]; ok {
		return a, nil
	}
	p, ok := r.profiles[name]
	if !ok {
		return nil, configErr("unknown auth profile %q", name)
	}
	p, err := substitute(p, r.vars)
	if err != nil {
		return nil, configErr("auth profile %q: %v", name, err)
	}
	if r.opts.Untrusted {
		// names can be templated, so check again now that they are known
		if bad := disallowedEnv(p, r.opts.AllowEnv); len(bad) > 0 {
			return nil, configErr("auth profile %q reads environment variable(s) %s, which untrusted mode does not allow",
				name, strings.Join(bad, ", "))
		}
		if p.Type == AuthExec {
			return nil, configErr("auth profile %q runs a command, which untrusted mode does not allow", name)
		}
	}
	a, err := buildAuth(p, AuthEnv{Client: r.client, Dir: r.dir})
	if err != nil {
		return nil, configErr("auth profile %q: %v", name, err)
	}
	stateful := !statelessAuth(p.Type)
	r.trackAuth(a, stateful)
	if stateful {
		r.auths[name] = a
		r.logf("Using auth profile %q (%s)\n", name, p.Type)
	}
	return a, nil
}

func (r *runner) assign(step Step, result any) error {
	for _, a := range step.Assignments {
		v, err := jsonpath.Get(a.JSONPath, result)
		if err != nil {
			return fmt.Errorf("unable to get value for var %s at path %s. %w", a.Var, a.JSONPath, err)
		}
		r.vars[a.Var] = v
	}
	return nil
}

func (r *runner) checkFailureConditions(step Step) error {
	for _, c := range step.FailureConditions {
		got, err := evaluate(c.Condition, r.vars)
		if err != nil {
			return fmt.Errorf("failure condition %s failed to evaluate. %w", c.Condition, err)
		}
		if matches(c.ExpectedValue, got) {
			return fmt.Errorf("failure condition %s met with observed value %v", c.Condition, got)
		}
	}
	return nil
}

func (s Step) checkStatus(resp *Response) error {
	method := "GET"
	url := ""
	if s.Request != nil {
		method, url = orDefault(strings.ToUpper(s.Request.Method), "GET"), s.Request.URL
	}
	if s.ExpectedHTTPStatus != 0 {
		if resp.StatusCode != s.ExpectedHTTPStatus {
			return fmt.Errorf("%s %s response status was %s. Expected %d.%s %s",
				method, url, resp.Status, s.ExpectedHTTPStatus, redirectNote(resp), truncate(string(resp.Body), 500))
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s response status was %s. Expected 2xx.%s %s",
			method, url, resp.Status, redirectNote(resp), truncate(string(resp.Body), 500))
	}
	return nil
}

// redirectNote explains a 3xx result: it means the redirect was not followed.
func redirectNote(resp *Response) string {
	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
		return fmt.Sprintf(" The redirect to %s was not followed (requests with credentials, or with followRedirects: false, do not follow redirects off-host).", loc)
	}
	return ""
}

func (r *runner) preview(raw []byte, parsed any) string {
	var b bytes.Buffer
	b.WriteString("Result\n")
	switch {
	case r.opts.Verbose && parsed != nil:
		pretty, err := json.MarshalIndent(parsed, "  ", "  ")
		if err == nil {
			b.Write(pretty)
		} else {
			b.Write(raw)
		}
	case r.opts.Verbose:
		b.Write(raw)
	default:
		text := strings.TrimSpace(string(raw))
		var compact bytes.Buffer
		if parsed != nil && json.Compact(&compact, raw) == nil {
			text = compact.String()
		}
		b.WriteString(truncate(text, 200))
	}
	b.WriteString("\n")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...."
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
