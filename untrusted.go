package hisepress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// UntrustedError is returned by Run, before anything executes, when a suite
// does something Options.Untrusted does not allow.
type UntrustedError struct{ Reasons []string }

func (e *UntrustedError) Error() string {
	return "suite refused in untrusted mode: " + strings.Join(e.Reasons, "; ")
}

// Untrusted mode is for running suites written by someone you don't trust,
// for instance a pull request in CI. It is risk reduction, not a sandbox: a
// suite can still make requests to any public host, so don't expose secrets
// you aren't prepared to have a suite use. In this mode:
//
//   - exec steps and exec auth profiles are refused;
//   - auth profiles may read only the environment variables named in
//     Options.AllowEnv (--allow-env), so a suite can't exfiltrate arbitrary
//     secrets from the environment;
//   - request files must lie inside the suite file's directory (symlinks
//     are resolved before checking);
//   - connections to link-local addresses (cloud metadata services such as
//     169.254.169.254) are refused. This applies only when Run builds its own
//     HTTP client, i.e. Options.HTTPClient is nil.

// untrustedViolations lists the static problems with running s in untrusted
// mode. Problems that depend on values only known at run time (templated
// environment variable names, file paths) are enforced again as steps run.
func (s *Suite) untrustedViolations(allowed []string) []string {
	var out []string
	if s.hasExec() {
		for _, st := range s.Steps {
			if st.Exec != nil {
				out = append(out, "it has exec steps")
				break
			}
		}
		for name, p := range s.Auth {
			if p.Type == AuthExec {
				out = append(out, fmt.Sprintf("auth profile %q runs a command", name))
			}
		}
	}
	for name, p := range s.Auth {
		if v := disallowedEnv(p, allowed); len(v) > 0 {
			out = append(out, fmt.Sprintf("auth profile %q reads environment variable(s) %s (allow with --allow-env)",
				name, strings.Join(v, ", ")))
		}
	}
	return out
}

// envRefs returns the environment variables an auth profile reads.
func envRefs(p AuthProfile) []string {
	var out []string
	for _, v := range []string{p.TokenEnv, p.UsernameEnv, p.PasswordEnv, p.ValueEnv, p.ClientIDEnv, p.ClientSecretEnv} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func disallowedEnv(p AuthProfile, allowed []string) []string {
	var bad []string
	for _, ref := range envRefs(p) {
		ok := false
		for _, a := range allowed {
			if a == ref {
				ok = true
				break
			}
		}
		if !ok {
			bad = append(bad, ref)
		}
	}
	return bad
}

// resolveFiles makes upload paths relative to the suite file's directory (the
// same base exec commands use). In untrusted mode it also refuses paths that
// leave that directory.
func (r *runner) resolveFiles(req *Request) error {
	if len(req.Files) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(req.Files))
	for field, p := range req.Files {
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.dir, p)
		}
		resolved[field] = p
		if r.opts.Untrusted {
			if err := insideDir(r.dir, p); err != nil {
				return configErr("file %q for field %q: %v", req.Files[field], field, err)
			}
		}
	}
	req.Files = resolved
	return nil
}

func insideDir(dir, path string) error {
	if dir == "" {
		dir = "."
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("does not exist")
		}
		return err
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("is outside the suite's directory, which untrusted mode does not allow")
	}
	return nil
}

// untrustedClient is the default HTTP client in untrusted mode. It refuses
// to connect to link-local addresses, after DNS resolution, so a hostname
// that resolves to a metadata service is caught too.
func untrustedClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && blockedAddress(ip) {
				return fmt.Errorf("untrusted mode refuses to connect to %s", ip)
			}
			return nil
		},
	}).DialContext
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

var awsMetadataV6 = net.ParseIP("fd00:ec2::254")

func blockedAddress(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.Equal(awsMetadataV6)
}
