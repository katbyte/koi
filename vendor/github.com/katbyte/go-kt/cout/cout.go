// Package cout provides verbosity-levelled, coloured console output for
// command-line tools.
//
// Tools separate what they *log* (clog, stderr, for diagnosing the tool) from
// what they *print* (this package, for the person running it). Every print
// call carries a minimum verbosity so a --quiet or --verbose flag is honoured
// in one place, and colour tags such as <red>...</> are rendered by
// gookit/color, which strips them when stdout is not a colour terminal.
//
// Tags are rendered anywhere in the final string, arguments included, so
// helpers may build coloured fragments and pass them through %s.
package cout

import (
	"io"
	"os"

	c "github.com/gookit/color"
)

// Verbosity is how much a tool prints. The levels are ordered, so "print at
// Normal and above" is a plain comparison.
type Verbosity int

// The verbosity levels, from least to most output.
const (
	// VerbositySilent prints nothing at all, not even errors. For callers that
	// only want the exit code.
	VerbositySilent Verbosity = iota
	// VerbosityJSON is for tools that emit a JSON document on stdout at the
	// end of a run: nothing else is printed there, errors still go to Err. The
	// document itself is the tool's to write; this level only keeps the
	// channel clean.
	VerbosityJSON
	// VerbosityQuiet prints only the minimal machine-readable lines (Quietf,
	// QuietOnlyf) and errors.
	VerbosityQuiet
	// VerbosityNormal is the default: everything a person wants to see.
	VerbosityNormal
	// VerbosityVerbose adds the detail behind Verbosef, typically -v.
	VerbosityVerbose
)

// String returns the level's name in lower case, matching the flag that
// usually selects it.
func (v Verbosity) String() string {
	switch v {
	case VerbositySilent:
		return "silent"
	case VerbosityJSON:
		return "json"
	case VerbosityQuiet:
		return "quiet"
	case VerbosityNormal:
		return "normal"
	case VerbosityVerbose:
		return "verbose"
	default:
		return "unknown"
	}
}

// Level controls the output verbosity. Tools set it once from their flags
// before any output call; it is a plain variable rather than a setter so the
// cobra flag-handling block in every tool stays a one-line assignment.
var Level = VerbosityNormal

// Out is where normal output goes. It defaults to stdout; tools whose stdout is
// a data channel (a JSON emitter, an MCP server on stdio) point it at stderr so
// progress messages never corrupt the stream.
var Out io.Writer = os.Stdout

// Err is where Errorf writes. It defaults to stderr and is separate from Out so
// errors stay visible when Out is redirected or discarded.
var Err io.Writer = os.Stderr

// printer snapshots the package state so each call reads the globals once and
// so the behaviour can be tested in parallel without touching them.
type printer struct {
	level Verbosity
	out   io.Writer
	err   io.Writer
}

func current() printer {
	return printer{level: Level, out: Out, err: Err}
}

// Writer returns Out when Level is Normal or above and io.Discard below, for
// code that streams output through something else (a tabwriter, an encoder)
// and cannot go through Printf.
func Writer() io.Writer {
	return current().writer()
}

func (p printer) writer() io.Writer {
	if p.level < VerbosityNormal {
		return io.Discard
	}
	return p.out
}

// Sprintf formats like fmt.Sprintf and renders colour tags in the result. Use
// it to build coloured fragments that are later passed to Printf and friends,
// or to colour text destined for somewhere other than Out.
func Sprintf(format string, args ...any) string {
	return c.Sprintf(format, args...)
}

// Printf prints normal output; suppressed in quiet and silent modes. Console
// write failures are not actionable, so they are dropped.
func Printf(format string, args ...any) {
	current().printf(VerbosityNormal, format, args...)
}

// Println prints normal output followed by a newline, rendering colour tags in
// its arguments; suppressed in quiet and silent modes.
func Println(args ...any) {
	p := current()
	if p.level < VerbosityNormal {
		return
	}
	c.Fprintln(p.out, args...)
}

// Verbosef prints detail that only matters when someone asked for it with -v;
// suppressed at Normal and below.
func Verbosef(format string, args ...any) {
	current().printf(VerbosityVerbose, format, args...)
}

// Quietf prints in quiet mode and above. Use it for the one line a script
// would parse, which should also appear in normal output alongside any
// decoration Printf adds around it.
func Quietf(format string, args ...any) {
	current().printf(VerbosityQuiet, format, args...)
}

// QuietOnlyf prints only in quiet mode. Use it when quiet mode has its own
// terse format for a line that normal mode prints differently via Printf, so
// the two never appear together.
func QuietOnlyf(format string, args ...any) {
	p := current()
	if p.level != VerbosityQuiet {
		return
	}
	c.Fprintf(p.out, format, args...)
}

// Errorf prints an error to Err in every mode except silent, so failures stay
// visible even when Out is machine-readable (quiet) or suppressed.
func Errorf(format string, args ...any) {
	p := current()
	if p.level == VerbositySilent {
		return
	}
	c.Fprintf(p.err, format, args...)
}

// printf writes to out when the level is at least minimum.
func (p printer) printf(minimum Verbosity, format string, args ...any) {
	if p.level < minimum {
		return
	}
	c.Fprintf(p.out, format, args...)
}
