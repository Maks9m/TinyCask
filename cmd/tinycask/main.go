// Command tinycask runs the TinyCask key-value server.
// Only -version exists so far; server flags (ARCHITECTURE §12) come later.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

const version = "v0.1.0-dev"

const (
	exitOK    = 0
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command and returns the exit code, so it is testable
// without os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tinycask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		if _, err := fmt.Fprintf(stderr, "tinycask: unexpected argument %q\n", fs.Arg(0)); err != nil {
			return exitUsage
		}
		fs.Usage()
		return exitUsage
	}

	if *showVersion {
		if _, err := fmt.Fprintln(stdout, versionString()); err != nil {
			return exitUsage
		}
		return exitOK
	}

	if _, err := fmt.Fprintln(stderr, "tinycask: server not implemented yet; only -version is supported"); err != nil {
		return exitUsage
	}
	return exitUsage
}

// versionString returns "tinycask <version>", plus the git revision if known.
func versionString() string {
	s := "tinycask " + version
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return s
	}
	var rev, dirty string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	const shortRevLen = 12
	if len(rev) > shortRevLen {
		rev = rev[:shortRevLen]
	}
	if rev != "" {
		s += " (" + rev + dirty + ")"
	}
	return s
}
