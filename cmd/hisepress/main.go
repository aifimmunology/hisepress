// Command hisepress runs API integration test suites.
//
//	hisepress run [flags] <suite file or directory>
//	hisepress validate <suite file or directory>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aifimmunology/hisepress"
)

const envVarPrefix = "HISEPRESS_VAR_"

var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// Exit codes: 0 all suites passed, 1 a suite failed, 2 usage or load error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "run":
		return runCmd(args[1:], stdout, stderr)
	case "validate":
		return validateCmd(args[1:], stdout, stderr)
	case "schema":
		b, err := hisepress.SuiteSchema()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "hisepress", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `hisepress - declarative API integration tests

Usage:
  hisepress run [flags] <suite file | directory>
  hisepress validate <suite file | directory>
  hisepress schema                 print the JSON Schema for suite files
  hisepress version

Run flags:
  --tag TAG        only run suites with this tag (repeatable; all must match)
  --var NAME=VALUE override a suite variable (repeatable). Variables can also
                   be set with HISEPRESS_VAR_<NAME> environment variables.
  --report FORMAT  what to print on stdout: console (default), json or junit
  --out DIR        also write per-suite logs, results.json and junit.xml to DIR;
                   steps that produce files write them there too
  --untrusted      run a suite you did not write more safely: refuses exec steps
                   and exec auth, limits environment access to --allow-env,
                   confines uploaded files to the suite's directory, and blocks
                   connections to cloud metadata addresses. Not a sandbox.
  --allow-env NAME with --untrusted, let auth profiles read this environment
                   variable (repeatable)
  --verbose        log full responses
  --debug          print diagnostic logs to stderr
`)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var tags, vars, allowEnv multiFlag
	fs.Var(&tags, "tag", "")
	fs.Var(&vars, "var", "")
	fs.Var(&allowEnv, "allow-env", "")
	report := fs.String("report", "console", "")
	outDir := fs.String("out", "", "")
	verbose := fs.Bool("verbose", false, "")
	debug := fs.Bool("debug", false, "")
	untrusted := fs.Bool("untrusted", false, "")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "run needs exactly one suite file or directory")
		return 2
	}
	if *report != "console" && *report != "json" && *report != "junit" {
		fmt.Fprintf(stderr, "unknown report format %q\n", *report)
		return 2
	}

	// --var is explicit, so it must name a variable some suite defines (this
	// catches typos); HISEPRESS_VAR_* come from the environment and apply
	// only where they match.
	explicit := map[string]string{}
	for _, kv := range vars {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			fmt.Fprintf(stderr, "--var %q must look like NAME=VALUE\n", kv)
			return 2
		}
		explicit[k] = v
	}
	fromEnv := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, envVarPrefix) {
			fromEnv[strings.TrimPrefix(k, envVarPrefix)] = v
		}
	}

	suites, err := loadTarget(fs.Arg(0), tags)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(suites) == 0 {
		fmt.Fprintln(stderr, "no suites matched")
		return 2
	}
	for k := range explicit {
		found := false
		for _, s := range suites {
			found = found || definesVar(s, k)
		}
		if !found {
			fmt.Fprintf(stderr, "--var %s: no suite defines a variable with that name\n", k)
			return 2
		}
	}

	level := slog.LevelError
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// the first Ctrl-C cancels the run (cleanup steps still execute); a second
	// one, with default handling restored, kills the process
	go func() { <-ctx.Done(); stop() }()

	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}

	results := []*hisepress.Result{}
	exit := 0
	for i, s := range suites {
		overrides := map[string]string{}
		for _, src := range []map[string]string{fromEnv, explicit} {
			for k, v := range src {
				if definesVar(s, k) {
					overrides[k] = v
				}
			}
		}
		opts := hisepress.Options{
			Logger: logger, Overrides: overrides, Verbose: *verbose, OutDir: *outDir,
			Untrusted: *untrusted, AllowEnv: allowEnv,
		}
		var writers []io.Writer
		if *report == "console" {
			fmt.Fprintf(stdout, "\n=== %s ===\n", s.Name)
			writers = append(writers, stdout)
		}
		var logFile *os.File
		if *outDir != "" {
			name := fmt.Sprintf("%02d-%s.log", i+1, slug(s.Name))
			f, err := os.OpenFile(filepath.Join(*outDir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				fmt.Fprintln(stderr, err)
				exit = 2
				break
			}
			logFile = f
			writers = append(writers, f)
		}
		if len(writers) > 0 {
			opts.Output = io.MultiWriter(writers...)
		}
		res, err := s.Run(ctx, opts)
		if logFile != nil {
			logFile.Close()
		}
		if err != nil {
			// keep what earlier suites produced, but stop and report the error
			fmt.Fprintf(stderr, "%s: %v\n", s.Name, err)
			exit = 2
			break
		}
		results = append(results, res)
		if !res.Passed() && exit == 0 {
			exit = 1
		}
	}

	if *outDir != "" && len(results) > 0 {
		if err := writeReports(*outDir, results); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}

	switch *report {
	case "json":
		if err := writeJSON(stdout, results); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	case "junit":
		if err := hisepress.WriteJUnit(stdout, results); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	default:
		fmt.Fprintln(stdout, "\nSummary")
		for _, r := range results {
			fmt.Fprintf(stdout, "  %-5s %s (%dms)\n", strings.ToUpper(r.Status), r.Suite, r.DurationMS)
		}
		if *outDir != "" {
			fmt.Fprintf(stdout, "\nLogs and reports written to %s\n", *outDir)
		}
	}
	return exit
}

func definesVar(s *hisepress.Suite, name string) bool {
	for k := range s.Variables {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func writeJSON(w io.Writer, results []*hisepress.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func writeReports(dir string, results []*hisepress.Result) error {
	jf, err := os.OpenFile(filepath.Join(dir, "results.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer jf.Close()
	if err := writeJSON(jf, results); err != nil {
		return err
	}
	xf, err := os.OpenFile(filepath.Join(dir, "junit.xml"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer xf.Close()
	return hisepress.WriteJUnit(xf, results)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "suite"
	}
	return s
}

func validateCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "validate needs exactly one suite file or directory")
		return 2
	}
	suites, err := loadTarget(args[0], nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	for _, s := range suites {
		fmt.Fprintf(stdout, "ok  %s (%d steps)\n", s.File, len(s.Steps))
	}
	return 0
}

func loadTarget(path string, tags []string) ([]*hisepress.Suite, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return hisepress.LoadSuites(path, tags...)
	}
	s, err := hisepress.LoadSuite(path)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if !s.HasTag(t) {
			return nil, nil
		}
	}
	return []*hisepress.Suite{s}, nil
}

// reorderArgs moves flags in front of positional arguments so that
// "hisepress run suite.yaml --tag x" works as well as "run --tag x suite.yaml".
func reorderArgs(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--verbose" || a == "-verbose" || a == "--debug" || a == "-debug" ||
			a == "--untrusted" || a == "-untrusted":
			flags = append(flags, a)
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			flags = append(flags, a)
		case strings.HasPrefix(a, "-") && i+1 < len(args):
			flags = append(flags, a, args[i+1])
			i++
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}
