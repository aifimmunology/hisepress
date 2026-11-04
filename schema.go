package hisepress

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// SuiteSchema returns a JSON Schema (draft 2020-12) describing suite files.
// It is generated from the Suite type, so it follows the code; field
// descriptions come from schemaDocs, and a test fails if one is missing.
//
// Editors can use it for validation and completion. For YAML, start the file
// with:  # yaml-language-server: $schema=path/to/suite.schema.json
func SuiteSchema() ([]byte, error) {
	g := &schemaGen{defs: map[string]any{}}
	root := g.structSchema(reflect.TypeOf(Suite{}))
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["title"] = "hisepress suite"
	root["description"] = "A hisepress test suite: variables, authentication profiles and an ordered list of steps."
	root["$defs"] = g.defs
	return json.MarshalIndent(root, "", "  ")
}

type schemaGen struct{ defs map[string]any }

func (g *schemaGen) schemaFor(t reflect.Type) any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		name := t.Name()
		if _, done := g.defs[name]; !done {
			g.defs[name] = nil // reserve, in case of recursion
			g.defs[name] = g.structSchema(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": g.schemaFor(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schemaFor(t.Elem())}
	default: // interface{}
		return true
	}
}

func (g *schemaGen) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if !f.IsExported() || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		prop := g.schemaFor(f.Type)
		pm, isMap := prop.(map[string]any)
		if !isMap { // "true" schema (any): wrap so it can carry a description
			pm = map[string]any{}
		}
		if doc, ok := schemaDocs[t.Name()+"."+name]; ok {
			pm["description"] = doc
		}
		props[name] = pm
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	if extra, ok := schemaExtras[t.Name()]; ok {
		extra(s)
	}
	return s
}

// schemaExtras adds constraints that cannot be derived from the Go types.
var schemaExtras = map[string]func(map[string]any){
	"Suite": func(s map[string]any) {
		s["properties"].(map[string]any)["steps"].(map[string]any)["minItems"] = 1
	},
	"Step": func(s map[string]any) {
		s["not"] = map[string]any{"anyOf": []any{
			map[string]any{"required": []string{"request", "code"}},
			map[string]any{"required": []string{"request", "exec"}},
			map[string]any{"required": []string{"code", "exec"}},
		}}
		p := s["properties"].(map[string]any)
		p["expectedHTTPStatus"].(map[string]any)["minimum"] = 100
		p["expectedHTTPStatus"].(map[string]any)["maximum"] = 599
	},
	"Condition": func(s map[string]any) {
		// expectedValue may be omitted (it then defaults to true)
		var req []string
		for _, n := range s["required"].([]string) {
			if n != "expectedValue" {
				req = append(req, n)
			}
		}
		s["required"] = req
	},
	"Exec": func(s map[string]any) {
		s["properties"].(map[string]any)["command"].(map[string]any)["minItems"] = 1
	},
	"AuthProfile": func(s map[string]any) {
		p := s["properties"].(map[string]any)
		p["type"].(map[string]any)["examples"] = []string{AuthBearer, AuthBasic, AuthHeader, AuthOAuth2, AuthExec}
		p["authStyle"].(map[string]any)["enum"] = []string{"header", "body"}
		p["command"].(map[string]any)["minItems"] = 1
		isType := func(t string) map[string]any {
			return map[string]any{"properties": map[string]any{"type": map[string]any{"const": t}}, "required": []string{"type"}}
		}
		anyOf := func(names ...string) map[string]any {
			var alts []any
			for _, n := range names {
				alts = append(alts, map[string]any{"required": []string{n}})
			}
			return map[string]any{"anyOf": alts}
		}
		allOf := func(parts ...map[string]any) map[string]any {
			var l []any
			for _, p := range parts {
				l = append(l, p)
			}
			return map[string]any{"allOf": l}
		}
		s["allOf"] = []any{
			map[string]any{"if": isType(AuthBearer), "then": anyOf("token", "tokenEnv")},
			map[string]any{"if": isType(AuthBasic), "then": allOf(anyOf("username", "usernameEnv"), anyOf("password", "passwordEnv"))},
			map[string]any{"if": isType(AuthHeader), "then": allOf(map[string]any{"required": []string{"header"}}, anyOf("value", "valueEnv"))},
			map[string]any{"if": isType(AuthOAuth2), "then": allOf(map[string]any{"required": []string{"tokenUrl"}},
				anyOf("clientId", "clientIdEnv"), anyOf("clientSecret", "clientSecretEnv"))},
			map[string]any{"if": isType(AuthExec), "then": map[string]any{"required": []string{"command"}}},
		}
	},
}

// schemaDocs describes every field, keyed "Type.jsonName".
var schemaDocs = map[string]string{
	"Suite.name":        "Display name. Defaults to the file name.",
	"Suite.description": "What the suite tests.",
	"Suite.tags":        "Labels for selecting suites with --tag. Matching ignores case and punctuation.",
	"Suite.variables":   "Starting variables. Strings may use <var name=x /> to reference other variables or built-ins (Timestamp, RandomInt, RandomString). Override with --var or HISEPRESS_VAR_<NAME>.",
	"Suite.auth":        "Named authentication profiles. Steps select one with their auth field.",
	"Suite.defaultAuth": "Name of the auth profile used by request steps that don't set auth.",
	"Suite.secrets":     "Names of variables whose values are redacted from logs and reports.",
	"Suite.steps":       "Steps, run in order.",

	"Step.name":                  "Step name, shown in logs and reports.",
	"Step.description":           "Free text, logged when the step runs. May use <var name=x />.",
	"Step.request":               "An HTTP request to make.",
	"Step.code":                  "Call a Go function registered with hisepress.Register.",
	"Step.exec":                  "Run an external program; its JSON output is the step result.",
	"Step.auth":                  "Auth profile for this request: a name from the suite's auth map, or \"none\". Defaults to defaultAuth.",
	"Step.wait":                  "Retry until the step passes, with exponential backoff (1s up to 1m), until timeoutInMinutes or a failure condition. Not valid with code or exec.",
	"Step.timeoutInMinutes":      "How long wait may keep retrying. Default 60.",
	"Step.alwaysRun":             "Run even if an earlier step failed (for cleanup).",
	"Step.alwaysContinue":        "A failure of this step does not fail the suite.",
	"Step.expectedHTTPStatus":    "Exact status the response must have. Default: any 2xx.",
	"Step.expectedResultsLength": "Length a JSON array response must have.",
	"Step.assignments":           "Values to copy from the result into variables.",
	"Step.conditions":            "Expressions that must evaluate to their expected values for the step to pass.",
	"Step.skipConditions":        "The step is skipped when any of these evaluates to its expected value.",
	"Step.failureConditions":     "While waiting, stop retrying and fail when any of these evaluates to its expected value.",

	"Request.url":             "URL. A missing scheme is https (http for localhost). May use <var name=x />.",
	"Request.method":          "HTTP method. Default GET.",
	"Request.params":          "Query parameters. Values are scalars, or lists of scalars for repeated keys.",
	"Request.headers":         "Request headers. These override headers set by an auth profile.",
	"Request.body":            "Request body: JSON for objects and lists, sent as-is for strings. With files, an object of extra form fields.",
	"Request.files":           "Multipart upload: form field name to a file path, relative to the suite file's directory.",
	"Request.followRedirects": "Follow redirects (default true). Requests carrying credentials never follow redirects to another host or from https to http. When a redirect is not followed, the 3xx response is the result.",

	"Code.name": "Name of a function registered with hisepress.Register.",
	"Code.args": "String arguments passed to the function.",

	"Exec.command":        "Program and arguments, run directly (no shell). Relative to the suite file's directory.",
	"Exec.args":           "Arguments passed to the program in the JSON document on stdin, and as HISEPRESS_ARG_<NAME> environment variables.",
	"Exec.env":            "Extra environment variables for the program.",
	"Exec.passVars":       "Include all suite variables in the stdin document. Off by default because variables may hold secrets.",
	"Exec.timeoutSeconds": "Kill the program after this long. Default 300.",

	"Condition.condition":     "Expression over suite variables, e.g. status == \"done\" && len(items) > 0.",
	"Condition.expectedValue": "Value the expression must evaluate to. Default true.",

	"Assignment.var":      "Variable to set.",
	"Assignment.jsonPath": "JSONPath into the step's result, e.g. $.items[0].id.",

	"AuthProfile.type":            "Profile type: bearer, basic, header, oauth2-client-credentials, exec, or a type added with RegisterAuth.",
	"AuthProfile.allowInsecure":   "Allow sending credentials over plain http to hosts other than localhost. Off by default.",
	"AuthProfile.hosts":           "Only send credentials to these hostnames (\"api.example.com\", or \"*.example.com\" for subdomains). Other hosts make the request fail.",
	"AuthProfile.header":          "Header to set. Default Authorization (required for type header).",
	"AuthProfile.scheme":          "Prefix before the token. Default Bearer; \"none\" sends the bare token.",
	"AuthProfile.token":           "Token. Prefer tokenEnv.",
	"AuthProfile.tokenEnv":        "Environment variable holding the token.",
	"AuthProfile.username":        "Username. Prefer usernameEnv.",
	"AuthProfile.usernameEnv":     "Environment variable holding the username.",
	"AuthProfile.password":        "Password. Prefer passwordEnv.",
	"AuthProfile.passwordEnv":     "Environment variable holding the password.",
	"AuthProfile.value":           "Header value. Prefer valueEnv.",
	"AuthProfile.valueEnv":        "Environment variable holding the header value.",
	"AuthProfile.tokenUrl":        "OAuth2 token endpoint.",
	"AuthProfile.clientId":        "OAuth2 client id. Prefer clientIdEnv.",
	"AuthProfile.clientIdEnv":     "Environment variable holding the client id.",
	"AuthProfile.clientSecret":    "OAuth2 client secret. Prefer clientSecretEnv.",
	"AuthProfile.clientSecretEnv": "Environment variable holding the client secret.",
	"AuthProfile.scope":           "OAuth2 scope.",
	"AuthProfile.audience":        "OAuth2 audience.",
	"AuthProfile.authStyle":       "Where to send client credentials: header (HTTP Basic, the default) or body.",
	"AuthProfile.command":         "Program that prints a token on stdout, or JSON {\"token\": \"...\", \"expiresIn\": 3600}.",
	"AuthProfile.cacheSeconds":    "How long to reuse a token printed by command. Default 300.",
	"AuthProfile.options":         "Settings for custom profile types registered with RegisterAuth.",
}

// schemaCheckDocs reports struct fields that have no description.
func schemaCheckDocs() []string {
	var missing []string
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if !f.IsExported() || tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if _, ok := schemaDocs[t.Name()+"."+name]; !ok {
				missing = append(missing, fmt.Sprintf("%s.%s", t.Name(), name))
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(Suite{}))
	return missing
}
