package issue

import (
	"bufio"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/cout"
)

// TestAskClose pins the one interactive path every close check runs through:
// accept must be distinguishable from every Apply* code (a check comparing it
// to the wrong one silently skips the close), and preview must loop back for a
// real answer rather than deciding anything itself. Not parallel — it swaps the
// package's stdin reader.
//
//nolint:paralleltest // swaps the package's stdin reader, so the cases run in order
func TestAskClose(t *testing.T) {
	cout.Level = cout.VerbositySilent
	t.Cleanup(func() { cout.Level = cout.VerbosityNormal })

	for _, tc := range []struct {
		name, keys string
		want       int
	}{
		{"accept", "a\n", AskAccept},
		{"enter is skip", "\n", ApplySkipped},
		{"preview then skip", "p\ns\n", ApplySkipped},
		{"preview then accept", "p\na\n", AskAccept},
		{"quit", "q\n", ApplyQuit},
		{"unknown key asks again", "z\ns\n", ApplySkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdinReader = bufio.NewReader(strings.NewReader(tc.keys))
			got, err := AskClose("close <cyan>#1</>?", "the comment that would be posted", "https://example.invalid/1")
			if err != nil {
				t.Fatalf("AskClose: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
