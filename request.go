package hisepress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxResponseBytes = 32 << 20
	maxRedirects     = 10
)

var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// configError marks a failure that retrying cannot fix (a bad URL, a missing
// environment variable, a credential policy violation). Steps with wait set
// fail immediately on these instead of retrying until they time out.
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

func configErr(format string, args ...any) error {
	return &configError{fmt.Errorf(format, args...)}
}

func isConfigError(err error) bool {
	var c *configError
	return errors.As(err, &c)
}

// Authenticator decorates outgoing requests, typically with a token.
type Authenticator interface {
	Apply(ctx context.Context, req *http.Request) error
}

// Response is the result of a Request.
type Response struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       []byte
}

// normalizeURL fills in a missing scheme: http for localhost, https otherwise.
func normalizeURL(raw string) string {
	if schemeRe.MatchString(raw) {
		return raw
	}
	host := raw
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "[::1]":
		return "http://" + raw
	}
	return "https://" + raw
}

// buildURL applies params to the URL. A query string already in the URL is
// sent exactly as written; params are appended to it.
func (r *Request) buildURL() (string, error) {
	u, err := url.Parse(normalizeURL(r.URL))
	if err != nil {
		return "", configErr("cannot parse url %q: %v", r.URL, err)
	}
	if len(r.Params) > 0 {
		q := url.Values{}
		for k, val := range r.Params {
			switch v := val.(type) {
			case []any:
				for _, item := range v {
					q.Add(k, stringify(item))
				}
			case map[string]any:
				return "", configErr("query param %s is an object; params must be scalars or lists", k)
			default:
				q.Add(k, stringify(v))
			}
		}
		if u.RawQuery == "" {
			u.RawQuery = q.Encode()
		} else {
			u.RawQuery += "&" + q.Encode()
		}
	}
	return u.String(), nil
}

// redirectPolicy decides whether to follow a redirect. A request that carries
// credentials never follows a redirect to another host, or from https to
// http: Go forwards custom headers across hosts, which would leak them. In
// those cases (and always when followRedirects is false) the 3xx response
// itself is the result.
func (r *Request) redirectPolicy(credentialed bool, inner func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	noFollow := r.FollowRedirects != nil && !*r.FollowRedirects
	return func(req *http.Request, via []*http.Request) error {
		if noFollow {
			return http.ErrUseLastResponse
		}
		if credentialed {
			first := via[0].URL
			if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
				return http.ErrUseLastResponse
			}
		}
		if inner != nil {
			return inner(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// Do performs the request.
func (r *Request) Do(ctx context.Context, client *http.Client, auth Authenticator) (*Response, error) {
	target, err := r.buildURL()
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	contentType := ""
	switch {
	case len(r.Files) > 0:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if fields, ok := r.Body.(map[string]any); ok {
			for k, v := range fields {
				if err := mw.WriteField(k, stringify(v)); err != nil {
					return nil, err
				}
			}
		} else if r.Body != nil {
			return nil, configErr("a request with files can only have an object body (form fields)")
		}
		for field, path := range r.Files {
			if err := addFile(mw, field, path); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		body, contentType = &buf, mw.FormDataContentType()
	case r.Body == nil:
	default:
		if s, ok := r.Body.(string); ok {
			body = strings.NewReader(s)
		} else {
			b, err := json.Marshal(r.Body)
			if err != nil {
				return nil, configErr("cannot encode request body: %v", err)
			}
			body, contentType = bytes.NewReader(b), "application/json"
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, configErr("%v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth != nil {
		if err := auth.Apply(ctx, req); err != nil {
			if isConfigError(err) {
				return nil, err
			}
			return nil, fmt.Errorf("authentication failed: %w", err)
		}
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.CheckRedirect = r.redirectPolicy(auth != nil, client.CheckRedirect)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if len(b) > maxResponseBytes {
		return nil, fmt.Errorf("response body is larger than %d MB", maxResponseBytes>>20)
	}
	return &Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header, Body: b}, nil
}

func addFile(mw *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return configErr("cannot open file %s to add to request: %v", path, err)
	}
	defer f.Close()
	part, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, f)
	return err
}
