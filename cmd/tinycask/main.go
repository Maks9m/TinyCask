// Command tinycask runs the TinyCask key-value server.
//
// Only the -version flag exists so far; the server flags from ARCHITECTURE §12
// are wired in by later tickets.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// version is the release this source tree is working towards.
const version = "v0.1.0-dev"

// Exit codes returned by run.
const (
	exitOK    = 0
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and executes the command, writing normal output to stdout
// and diagnostics to stderr. It returns the process exit code so that it can
// be tested without calling os.Exit.
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

// versionString returns "tinycask <version>", plus the VCS revision when the
// binary was built from a git checkout.
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
