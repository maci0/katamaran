package clispec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testDoc() Doc {
	return New("widget", "v0.8.0", "A test command.", []Command{{
		Name:        "run",
		Description: "Do the thing. A retry is not safe.",
		Effects:     "non_idempotent",
		OutputKind:  "opaque",
		MediaType:   "text/plain",
	}}, Errors())
}

func TestSchemaDocument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	mode, _, code, handled := Gate([]string{"schema"}, &stdout, &stderr, "widget", "v0.8.0", testDoc())
	if !handled || code != 0 || mode != "auto" {
		t.Fatalf("schema gate: handled=%v code=%d mode=%s", handled, code, mode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("schema stderr: %s", stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"clispec", "name", "version", "commands", "errors", "output", "global_args"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if doc["clispec"] != "0.3" || doc["name"] != "widget" || doc["version"] != "v0.8.0" {
		t.Fatalf("identity: %#v", doc)
	}
	commands, _ := doc["commands"].([]any)
	if len(commands) != 3 {
		t.Fatalf("commands: %d", len(commands))
	}
}

func TestSchemaFilterAndUsageFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, _, code, handled := Gate([]string{"-o", "json", "schema", "run"}, &stdout, &stderr, "widget", "v0.8.0", testDoc())
	if !handled || code != 0 {
		t.Fatalf("filter: handled=%v code=%d stderr=%s", handled, code, stderr.String())
	}
	var doc struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Commands) != 1 || doc.Commands[0].Name != "run" {
		t.Fatalf("filter commands: %+v", doc.Commands)
	}

	stdout.Reset()
	stderr.Reset()
	_, _, code, _ = Gate([]string{"-o", "json", "schema", "nope"}, &stdout, &stderr, "widget", "v0.8.0", testDoc())
	if code != 2 {
		t.Fatalf("unknown path code %d", code)
	}
	Finish(&stderr, "json", code)
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	var env struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(last), &env); err != nil {
		t.Fatalf("envelope %q: %v", last, err)
	}
	if env.Error.Kind != "usage" {
		t.Fatalf("kind %s", env.Error.Kind)
	}
}

func TestVersionAndTextAreNotJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, _, code, handled := Gate([]string{"-o", "text", "version"}, &stdout, &stderr, "widget", "v0.8.0", testDoc())
	if !handled || code != 0 {
		t.Fatalf("version: handled=%v code=%d", handled, code)
	}
	if strings.HasPrefix(stdout.String(), "{") || stdout.String() != "widget v0.8.0\n" {
		t.Fatalf("text version: %q", stdout.String())
	}
	stdout.Reset()
	_, _, code, _ = Gate([]string{"-o", "json", "version"}, &stdout, &stderr, "widget", "v0.8.0", testDoc())
	if code != 0 || stdout.String() != "{\"version\":\"v0.8.0\"}\n" {
		t.Fatalf("json version: %q", stdout.String())
	}
}

func TestTakeOutputLeavesTheRest(t *testing.T) {
	mode, rest, bad := TakeOutput([]string{"--output=json", "--mode", "source"})
	if bad != "" || mode != "json" || strings.Join(rest, " ") != "--mode source" {
		t.Fatalf("mode=%s rest=%v bad=%s", mode, rest, bad)
	}
	_, _, bad = TakeOutput([]string{"-o"})
	if bad == "" {
		t.Fatal("missing value")
	}
}
