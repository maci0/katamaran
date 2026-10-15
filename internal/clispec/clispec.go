// Package clispec prints a CLI Spec 0.3 document and the shared -o / --output
// handling for the katamaran commands.
package clispec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Command is one invocable command in the spec.
type Command struct {
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	Effects      string  `json:"effects"`
	Cardinality  string  `json:"cardinality,omitempty"`
	OutputKind   string  `json:"output_kind,omitempty"`
	MediaType    string  `json:"media_type,omitempty"`
	StreamFormat string  `json:"stream_format,omitempty"`
	OutputFields []Field `json:"output_fields,omitempty"`
}

// Field is one JSON field a data command prints.
type Field struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Items json.RawMessage `json:"items,omitempty"`
}

// Arg is one global argument.
type Arg struct {
	Name        string   `json:"name"`
	Short       string   `json:"short,omitempty"`
	Type        string   `json:"type"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
	Description string   `json:"description"`
}

// ErrorKind is one declared process failure.
type ErrorKind struct {
	Kind        string `json:"kind"`
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

// Doc is the top-level CLI Spec document.
type Doc struct {
	Clispec     string `json:"clispec"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Output      struct {
		TTY   string `json:"tty"`
		Piped string `json:"piped"`
	} `json:"output"`
	GlobalArgs []Arg       `json:"global_args"`
	Commands   []Command   `json:"commands"`
	Errors     []ErrorKind `json:"errors"`
}

// Errors are the usage and failure kinds every command shares.
func Errors() []ErrorKind {
	return []ErrorKind{
		{Kind: "usage", ExitCode: 2, Description: "Unknown command, flag, or a rejected flag value."},
		{Kind: "failure", ExitCode: 1, Description: "The command was valid and did not finish."},
	}
}

// ErrorsWithSignal adds the signal exit used by the migration commands.
func ErrorsWithSignal() []ErrorKind {
	return append(Errors(), ErrorKind{
		Kind:        "interrupted",
		ExitCode:    130,
		Description: "The process received SIGINT or SIGTERM.",
	})
}

// New builds a document. schema and version are always invocable.
func New(name, version, description string, extra []Command, errors []ErrorKind) Doc {
	commands := make([]Command, 0, 2+len(extra))
	commands = append(commands, schemaCommand(), versionCommand())
	commands = append(commands, extra...)
	doc := Doc{
		Clispec:     "0.3",
		Name:        name,
		Version:     version,
		Description: description,
		GlobalArgs:  globalArgs(),
		Commands:    commands,
		Errors:      errors,
	}
	doc.Output.TTY = "text"
	doc.Output.Piped = "text"
	return doc
}

func schemaCommand() Command {
	return Command{
		Name:        "schema",
		Description: "Print this CLI spec document. Needs no config and no network.",
		Effects:     "read_only",
		Cardinality: "single",
		OutputFields: []Field{
			{Name: "clispec", Type: "string"},
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "commands", Type: "array", Items: json.RawMessage(`{"type":"object"}`)},
		},
	}
}

func versionCommand() Command {
	return Command{
		Name:         "version",
		Description:  "Print the product version.",
		Effects:      "read_only",
		Cardinality:  "single",
		OutputFields: []Field{{Name: "version", Type: "string"}},
	}
}

func globalArgs() []Arg {
	return []Arg{{
		Name:        "--output",
		Short:       "-o",
		Type:        "string",
		Enum:        []string{"auto", "text", "json"},
		Default:     "auto",
		Description: "Before the command. auto stays text when piped. Log format stays on stderr.",
	}}
}

// TakeOutput removes -o / --output from args. mode is auto when the flag is
// absent. bad is set when the value is missing or not auto, text, or json.
func TakeOutput(args []string) (mode string, rest []string, bad string) {
	mode = "auto"
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		value, ok, consumed, err := outputValue(arg, args, i)
		if err != "" {
			return "auto", nil, err
		}
		if ok {
			if value != "auto" && value != "text" && value != "json" {
				return "auto", nil, "Error: --output requires auto, text, or json"
			}
			mode = value
			i += consumed
			continue
		}
		rest = append(rest, arg)
	}
	return mode, rest, ""
}

func outputValue(arg string, args []string, i int) (value string, ok bool, consumed int, bad string) {
	switch {
	case arg == "-o" || arg == "--output":
		if i+1 >= len(args) {
			return "", false, 0, "Error: --output requires auto, text, or json"
		}
		return args[i+1], true, 1, ""
	case strings.HasPrefix(arg, "--output="):
		return strings.TrimPrefix(arg, "--output="), true, 0, ""
	case len(arg) > 2 && arg[0] == '-' && arg[1] == 'o' && !strings.HasPrefix(arg, "--"):
		return arg[2:], true, 0, ""
	default:
		return "", false, 0, ""
	}
}

// Gate handles schema, the version word, and a rejected --output.
// handled is true when the caller should exit with code.
// The caller prints the error envelope via Finish.
func Gate(args []string, stdout, stderr io.Writer, name, version string, doc Doc) (mode string, rest []string, code int, handled bool) {
	mode, rest, bad := TakeOutput(args)
	if bad != "" {
		fmt.Fprintln(stderr, bad)
		return "auto", nil, 2, true
	}
	if len(rest) > 0 && rest[0] == "schema" {
		return mode, nil, Emit(stdout, stderr, doc, rest[1:]), true
	}
	if len(rest) > 0 && rest[0] == "version" {
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "Error: unexpected arguments")
			return mode, nil, 2, true
		}
		writeVersion(stdout, mode, name, version)
		return mode, nil, 0, true
	}
	return mode, rest, 0, false
}

func writeVersion(stdout io.Writer, mode, name, version string) {
	if mode == "json" {
		fmt.Fprintf(stdout, "{\"version\":%q}\n", version)
		return
	}
	fmt.Fprintf(stdout, "%s %s\n", name, version)
}

// Emit writes the spec, or a usage line for schema --help.
func Emit(stdout, stderr io.Writer, doc Doc, path []string) int {
	for _, part := range path {
		if part == "--help" || part == "-h" || part == "help" {
			fmt.Fprintf(stdout, "usage: %s schema [command]\n", doc.Name)
			return 0
		}
		if strings.HasPrefix(part, "-") {
			fmt.Fprintf(stderr, "usage: %s schema [command]\n", doc.Name)
			return 2
		}
	}
	body, err := Render(doc, path)
	if err != nil {
		fmt.Fprintln(stderr, "unknown command path")
		return 2
	}
	_, _ = stdout.Write(body)
	return 0
}

// Render returns the document, filtered to path when path is not empty.
func Render(doc Doc, path []string) ([]byte, error) {
	if len(path) > 0 {
		prefix := strings.Join(path, " ")
		filtered := make([]Command, 0, len(doc.Commands))
		for _, cmd := range doc.Commands {
			if cmd.Name == prefix || strings.HasPrefix(cmd.Name, prefix+" ") {
				filtered = append(filtered, cmd)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("unknown command path")
		}
		doc.Commands = filtered
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Finish appends the error envelope when mode is json and code is a declared
// failure. Success and text mode write nothing.
func Finish(stderr io.Writer, mode string, code int) {
	if mode != "json" || code == 0 {
		return
	}
	kind, message := "", ""
	switch code {
	case 2:
		kind, message = "usage", "invalid arguments"
	case 1:
		kind, message = "failure", "command failed"
	case 130:
		kind, message = "interrupted", "interrupted by signal"
	default:
		return
	}
	fmt.Fprintf(stderr, "{\"error\":{\"kind\":%q,\"message\":%q}}\n", kind, message)
}
