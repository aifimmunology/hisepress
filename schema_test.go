package hisepress

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const schemaFile = "schema/suite.schema.json"

func compiledSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := SuiteSchema()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("suite.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("suite.schema.json")
	if err != nil {
		t.Fatalf("the generated schema is not a valid JSON Schema: %v", err)
	}
	return s
}

// validate checks a YAML or JSON document against the schema.
func validateDoc(t *testing.T, s *jsonschema.Schema, src string) error {
	t.Helper()
	var generic any
	if err := yaml.Unmarshal([]byte(src), &generic); err != nil {
		t.Fatalf("test document is not valid yaml: %v", err)
	}
	generic, err := jsonCompatible(generic)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(generic)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(inst)
}

// TestSchemaUpToDate fails when the committed schema no longer matches the Go
// types. Regenerate it with: UPDATE_SCHEMA=1 go test -run TestSchemaUpToDate
func TestSchemaUpToDate(t *testing.T) {
	want, err := SuiteSchema()
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("UPDATE_SCHEMA") != "" {
		if err := os.WriteFile(schemaFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date; run: UPDATE_SCHEMA=1 go test -run TestSchemaUpToDate", schemaFile)
	}
}

func TestSchemaDocumentsEveryField(t *testing.T) {
	if missing := schemaCheckDocs(); len(missing) > 0 {
		t.Fatalf("add descriptions to schemaDocs for: %s", strings.Join(missing, ", "))
	}
	// and no stale entries for fields that no longer exist
	known := map[string]bool{}
	b, _ := SuiteSchema()
	var root struct {
		Props map[string]any            `json:"properties"`
		Defs  map[string]map[string]any `json:"$defs"`
	}
	json.Unmarshal(b, &root)
	for p := range root.Props {
		known["Suite."+p] = true
	}
	for name, def := range root.Defs {
		for p := range def["properties"].(map[string]any) {
			known[name+"."+p] = true
		}
	}
	for key := range schemaDocs {
		if !known[key] {
			t.Errorf("schemaDocs has an entry for %s, which is not a field", key)
		}
	}
}

func TestShippedSuitesValidateAgainstSchema(t *testing.T) {
	s := compiledSchema(t)
	files, _ := filepath.Glob("examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no example suites found")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateDoc(t, s, string(src)); err != nil {
			t.Errorf("%s does not match the schema:\n%v", f, err)
		}
		// the schema and the loader should agree
		if _, err := LoadSuite(f); err != nil {
			t.Errorf("%s fails to load: %v", f, err)
		}
	}
}

func TestSchemaAcceptsAndRejects(t *testing.T) {
	s := compiledSchema(t)
	ok := map[string]string{
		"minimal":   "steps: [{request: {url: x}}]",
		"all kinds": "steps: [{request: {url: x}}, {code: {name: f}}, {exec: {command: [a]}}, {}]",
		"custom auth type": `
auth: {p: {type: my-plugin, options: {a: b}}}
steps: [{request: {url: x}, auth: p}]`,
		"oauth": `
auth: {p: {type: oauth2-client-credentials, tokenUrl: u, clientId: a, clientSecretEnv: B, authStyle: body}}
steps: [{request: {url: x}}]`,
		"any-typed fields": `
variables: {a: 1, b: [1, 2], c: {d: true}}
steps: [{request: {url: x, body: {n: 1}, params: {k: [a, b]}}, conditions: [{condition: x, expectedValue: 3}]}]`,
	}
	for name, src := range ok {
		if err := validateDoc(t, s, src); err != nil {
			t.Errorf("%s should be valid: %v", name, err)
		}
	}
	bad := map[string]string{
		"no steps":              "name: x",
		"empty steps":           "steps: []",
		"unknown suite field":   "stepz: []\nsteps: [{}]",
		"unknown step field":    "steps: [{requset: {url: x}}]",
		"unknown request field": "steps: [{request: {url: x, verb: GET}}]",
		"request and exec":      "steps: [{request: {url: x}, exec: {command: [a]}}]",
		"code and exec":         "steps: [{code: {name: f}, exec: {command: [a]}}]",
		"request without url":   "steps: [{request: {method: GET}}]",
		"empty exec command":    "steps: [{exec: {command: []}}]",
		"status out of range":   "steps: [{request: {url: x}, expectedHTTPStatus: 99}]",
		"assignment no path":    "steps: [{request: {url: x}, assignments: [{var: a}]}]",
		"condition no expr":     "steps: [{conditions: [{expectedValue: true}]}]",
		"auth no type":          "auth: {p: {token: x}}\nsteps: [{}]",
		"bearer without token":  "auth: {p: {type: bearer}}\nsteps: [{}]",
		"basic without pass":    "auth: {p: {type: basic, username: u}}\nsteps: [{}]",
		"header without header": "auth: {p: {type: header, value: v}}\nsteps: [{}]",
		"oauth without url":     "auth: {p: {type: oauth2-client-credentials, clientId: a, clientSecret: b}}\nsteps: [{}]",
		"exec auth no command":  "auth: {p: {type: exec}}\nsteps: [{}]",
		"bad auth style":        "auth: {p: {type: oauth2-client-credentials, tokenUrl: u, clientId: a, clientSecret: b, authStyle: x}}\nsteps: [{}]",
		"wrong type":            "steps: [{wait: \"yes\", request: {url: x}}]",
	}
	for name, src := range bad {
		if err := validateDoc(t, s, src); err == nil {
			t.Errorf("%s should be rejected by the schema", name)
		}
	}
}
