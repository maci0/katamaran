package buildinfo

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// CommonFlags holds the --version/-v/--help/-h flags every katamaran binary
// shares. Callers register them on their own FlagSet before parsing and hand
// the set to Act once it is parsed, so the help text, the version line, and
// the exit codes cannot drift between binaries.
type CommonFlags struct {
	version bool
	short   bool
	help    bool
	helpSh  bool
}

// Register adds the shared flags to fs.
func (c *CommonFlags) Register(fs *flag.FlagSet) {
	fs.BoolVar(&c.version, "version", false, "Show version and exit")
	fs.BoolVar(&c.short, "v", false, "")
	fs.BoolVar(&c.help, "help", false, "")
	fs.BoolVar(&c.helpSh, "h", false, "")
}

// Act handles the shared flags once fs has been parsed: it prints usage or
// the version line and reports the process exit code to use. It returns
// handled=false when the binary should keep running. usage prints the
// binary's own usage text.
func (c *CommonFlags) Act(fs *flag.FlagSet, name string, stdout, stderr io.Writer, usage func(io.Writer)) (exitCode int, handled bool) {
	if c.help || c.helpSh {
		usage(stdout)
		return 0, true
	}
	if c.version || c.short {
		_, _ = fmt.Fprintf(stdout, "%s %s\n", name, Version)
		return 0, true
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "Error: unexpected arguments: %s\n\n", strings.Join(fs.Args(), " "))
		usage(stderr)
		return 2, true
	}
	return 0, false
}
