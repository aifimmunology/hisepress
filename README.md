# hisepress

Declarative, multi-step API integration tests. Call an endpoint, capture values
from the response, feed them into the next call, assert on the results, and run
your own code in between.

Hisepress began as an internal tool for integration-testing the Allen
Institute's Human Immune System Explorer (HISE), hence the name. It has since
been generalized into a standalone tool for testing any HTTP API.

> **Status:** early. The engine, CLI (JUnit/JSON reports), exec steps and
> authentication profiles work and are tested. A JSON schema is published for
> editor support (see Roadmap for what's left).

```yaml
name: create and fetch
variables:
  base: https://api.example.com
  title: "widget-<var name=RandomString />"
steps:
  - name: create
    request: {url: "<var name=base />/items", method: POST, body: {name: "<var name=title />"}}
    expectedHTTPStatus: 201
    assignments: [{var: id, jsonPath: $.id}]
  - name: fetch it back
    request: {url: "<var name=base />/items/<var name=id />"}
    assignments: [{var: name, jsonPath: $.name}]
    conditions: [{condition: name == title, expectedValue: true}]
```

```
go install github.com/aifimmunology/hisepress/cmd/hisepress@latest
hisepress run suite.yaml            # or a directory of suites
hisepress run suites/ --tag smoke --var base=http://localhost:8080
hisepress run suites/ --out reports/ --report junit   # CI: JUnit on stdout, logs + results.json + junit.xml in reports/
hisepress validate suite.yaml
hisepress schema > suite.schema.json   # JSON Schema, for editor validation and completion
```

Try the example (needs network, no credentials): `hisepress run examples/httpbin.yaml`

`--out DIR` writes one log per suite plus `results.json` and `junit.xml`, and is
the directory code steps receive from `step.OutDir()` for any files they produce.
`--report` chooses what goes to stdout: `console` (default), `json`, or `junit`.

Exit codes: `0` all suites passed, `1` a suite failed, `2` usage or load error.

## Suite language

For editor validation and completion, point your editor at `schema/suite.schema.json`
(also printed by `hisepress schema`). In VS Code with the YAML extension, or any editor
using yaml-language-server, start the file with:

```yaml
# yaml-language-server: $schema=./path/to/suite.schema.json
```

(a relative path to the file, or a URL if you host it; see `examples/`).
The schema is generated from the Go types, and a test fails if the committed copy
is stale: `UPDATE_SCHEMA=1 go test -run TestSchemaUpToDate` regenerates it.

Suites are YAML or JSON (chosen by file extension). Unknown fields are
rejected. The loader accepts any capitalization of a field name, but the schema
checks the documented spelling, so stick to that.

| Suite field | |
|---|---|
| `name`, `description`, `tags` | `tags` select suites with `--tag`; matching ignores case and punctuation |
| `variables` | starting variables; strings may reference other variables |
| `auth`, `defaultAuth` | named [authentication profiles](#authentication) and the one steps use by default |
| `secrets` | names of variables whose values are redacted from logs and reports (see [Secrets and logs](#secrets-and-logs)) |
| `steps` | run in order |

| Step field | |
|---|---|
| `request` | `url`, `method`, `params`, `headers`, `body` (JSON for maps/lists, raw for strings), `files` (multipart; paths are relative to the suite file), `followRedirects` (default true; see [Redirects](#redirects)) |
| `code` | `{name, args}` calls a function registered from Go (see below) |
| `auth` | the profile for this request: a name from the suite's `auth`, or `none`; default is `defaultAuth` |
| `exec` | runs an external program (see [Exec steps](#exec-steps)); a step has at most one of `request`, `code`, `exec` |
| `assignments` | `{var, jsonPath}` copies a value from the response (or code result) into a variable |
| `conditions` | `{condition, expectedValue}`; the step fails unless the expression evaluates to the expected value (`true` if `expectedValue` is omitted) |
| `skipConditions` | same shape; the step is skipped when one matches |
| `failureConditions` | while waiting, stop retrying and fail when one matches |
| `expectedHTTPStatus` | exact status; default is any 2xx. To expect a 3xx, set `followRedirects: false` |
| `expectedResultsLength` | length the response, which must be a JSON array, must have |
| `wait`, `timeoutInMinutes` | retry with exponential backoff (1s up to 1m) until the step passes; default timeout 60 min |
| `alwaysRun` | run even if an earlier step failed (cleanup) |
| `alwaysContinue` | a failure here doesn't fail the suite |

**Variables.** `<var name=foo />` is replaced anywhere in a step. Built-ins:
`Timestamp`, `RandomInt`, `RandomString`; each has one value within a step, and
a fresh one in the next. Override suite variables with `--var name=value` or
`HISEPRESS_VAR_NAME=value`; values are converted to the type of the variable
they replace. `--var` must name a variable that some suite being run defines
(so a typo is an error, not a silent default); `HISEPRESS_VAR_*` settings that
match no variable are ignored, since the environment is shared by every suite.

Numbers pass through variables as 64-bit floats, so integers above 2^53 lose
precision. Carry such IDs as strings.

**Expressions** use [goval](https://github.com/maja42/goval) with suite
variables in scope, plus `len(x)`. Add functions through `hisepress.EvalFunctions`.

**Exec steps.** Run any program between requests: Python, bash, a compiled binary.

```yaml
- name: score it
  exec:
    command: [python3, scripts/score.py]  # run directly, no shell; relative to the suite file
    args: {sample: "<var name=sampleId />"}
    env: {API_BASE: "<var name=base />"}  # added to the inherited environment
    passVars: false                       # true adds all suite variables to stdin
    timeoutSeconds: 60                    # default 300
  assignments: [{var: score, jsonPath: $.score}]
```

The program reads one JSON document from stdin and prints one JSON document to
stdout, which becomes the step result (so `assignments` and `conditions` work as
for requests):

```python
import json, sys
req = json.load(sys.stdin)      # {"args": {...}, "outDir": "...", "vars": {...} if passVars}
print(json.dumps({"score": 0.97}))
```

- A non-zero exit fails the step. stderr is written to the run log and quoted in the error.
- Args are also available as `HISEPRESS_ARG_<NAME>`, and the output directory as
  `HISEPRESS_OUT_DIR` (set with `--out`), for scripts that would rather not parse JSON.
- Non-JSON output is fine if nothing needs to be assigned from it.
- Files can be attached to the run by returning `"_artifacts": [{"type": "...", "path": "..."}]`.
- Each exec step starts a new process; there is no persistent interpreter.
- A timeout or Ctrl-C kills the program and its child processes (on Windows, only the program itself).
- Variable values, including ones assigned from API responses, are substituted
  into `command` and `args` before the program runs. Never interpolate them into
  a shell string (`[bash, -c, "echo <var name=x />"]` is injectable from response
  data); pass them as `args` and read them from stdin or the environment instead.
- Exec runs arbitrary programs with your privileges. For suites you did not
  write, use [`--untrusted`](#running-suites-you-did-not-write), which refuses them.

See `examples/exec.yaml`.

### Authentication

Declare named profiles once and pick one per request:

```yaml
defaultAuth: svc
auth:
  svc:
    type: oauth2-client-credentials
    tokenUrl: https://login.example.com/oauth/token
    clientId: my-client
    clientSecretEnv: MY_CLIENT_SECRET     # secrets come from the environment
    scope: read
    hosts: ["*.example.com"]              # refuse to send this anywhere else
  admin: {type: bearer, tokenEnv: ADMIN_TOKEN}
steps:
  - request: {url: https://api.example.com/things}        # uses svc
  - auth: admin                                            # uses admin
    request: {url: https://api.example.com/admin/things}
  - auth: none                                             # sends nothing
    request: {url: https://api.example.com/public}
```

| `type` | settings |
|---|---|
| `bearer` | `token` or `tokenEnv`; `header` (default `Authorization`); `scheme` (default `Bearer`, or `none` for the bare token) |
| `basic` | `username`/`usernameEnv`, `password`/`passwordEnv` |
| `header` | `header`, `value` or `valueEnv`: API keys |
| `oauth2-client-credentials` | `tokenUrl`, `clientId`/`clientIdEnv`, `clientSecret`/`clientSecretEnv`, `scope`, `audience`, `authStyle` (`header` default, or `body`). Tokens are cached until shortly before they expire |
| `exec` | `command` prints a token on stdout, or JSON `{"token": "...", "expiresIn": 3600}`; `cacheSeconds` (default 300). Use this for anything else: `gcloud auth print-identity-token`, a vault CLI, your own script |

For every type, give each secret either literally or through its `...Env`
field, but not both. `hosts` restricts a profile to the listed hostnames
(`*.example.com` matches subdomains). Strings in profiles can use
`<var name=x />`, so a login step can assign a token that a `bearer` profile
then uses.

Credentials are never sent over plain `http://`, except to `localhost`, unless
the profile sets `allowInsecure: true`; the same applies to an OAuth2 `tokenUrl`.
A request that carries credentials does not follow redirects to another host or
from https to http (see [Redirects](#redirects)).

#### Redirects

Requests follow redirects by default, up to 10. Two exceptions return the 3xx
response itself as the step's result, so you can assert on it with
`expectedHTTPStatus: 302` (and read `Location` in the log):
`followRedirects: false`, and any request that carries credentials when the
redirect goes to a different host or downgrades https to http. Go forwards
custom headers across hosts on redirect, which is why credentials are not
allowed to follow. Secrets you put in `request.headers` yourself are not
covered by this rule: set `followRedirects: false` for those requests.

Embedders can add profile types with `hisepress.RegisterAuth(type, factory)`,
configured through the profile's `options` map, or set `Options.Auth` to
authenticate every request that has no profile. See `examples/auth.yaml`.

**Code steps.** Embed hisepress and register functions:

```go
hisepress.Register("MyFunc", func(ctx context.Context, step *hisepress.Step, args map[string]string) (any, error) {
    return map[string]any{"value": 42}, nil // assignments read this with jsonPath
})
suite, _ := hisepress.LoadSuite("suite.yaml")
result, _ := suite.Run(ctx, hisepress.Options{Output: os.Stdout})
```

## Secrets and logs

The run log records each request's method and URL, a preview of each response
body (all of it with `--verbose`), exec stderr, and error messages that quote
response bodies. The log is also embedded in JUnit reports and written by `--out`
(files are created 0600, the directory 0700). Hisepress redacts, as
`[REDACTED]`:

- the credentials held by auth profiles (tokens, passwords, API keys, OAuth2
  client secrets and the tokens fetched with them) and the header values built
  from them, when they are 8 characters or longer;
- the current value of every variable listed under the suite's `secrets`, at any
  length.

```yaml
secrets: [sessionToken]       # e.g. assigned by a login step
```

Redaction is best-effort: it matches exact strings, so a secret that a server
transforms before echoing it back is not caught. Treat logs from suites that
handle real credentials as sensitive anyway.

## Running suites you did not write

`hisepress run --untrusted` (`Options.Untrusted` in the library) is for suites
from someone you do not trust, such as a pull request in CI. It:

- refuses `exec` steps and `exec` auth profiles;
- lets auth profiles read only the environment variables you name with
  `--allow-env NAME` (repeatable), so a suite cannot send, say,
  `AWS_SECRET_ACCESS_KEY` to a server it controls;
- requires `request.files` to lie inside the suite file's directory (symlinks are
  resolved before checking);
- refuses connections to link-local addresses, including cloud metadata
  services such as 169.254.169.254 (only when hisepress builds its own HTTP
  client, that is, not with a custom `Options.HTTPClient`).

Problems that can be seen up front are reported before any step runs. This
reduces risk; it is not a sandbox. The suite can still make requests to any
public host and use any credential you allow, and it can reach private network
addresses. Do not allow secrets you are not prepared to have it use.

## Roadmap

1. Integers above 2^53 are not preserved through variables (`json.Number` end to end would fix it)
2. First tagged release: `.github/workflows/ci.yml` and `.goreleaser.yaml` are in place but have not yet run

## License

BSD 3-Clause. See [LICENSE](LICENSE).
