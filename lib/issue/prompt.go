package issue

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pkg/browser"
	"golang.org/x/term"

	"github.com/katbyte/go-kt/cout"
)

// stdinReader is shared so successive prompts don't lose buffered input.
var stdinReader = bufio.NewReader(os.Stdin)

// PromptInput prints prompt and returns the trimmed line the user entered.
func PromptInput(prompt string) (string, error) {
	cout.Printf("%s", prompt)
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// PromptKey prints prompt and returns a single pressed key — no enter needed.
// Enter returns "" (the default choice), ctrl-c aborts the run. When stdin
// isn't a terminal it falls back to line input so pipes keep working.
func PromptKey(prompt string) (string, error) {
	cout.Printf("%s", prompt)

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := stdinReader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("reading input: %w", err)
		}
		return strings.ToLower(strings.TrimSpace(line)), nil
	}

	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("entering raw mode: %w", err)
	}
	b, rerr := stdinReader.ReadByte()
	_ = term.Restore(fd, old)
	if rerr != nil {
		return "", fmt.Errorf("reading key: %w", rerr)
	}

	switch b {
	case 3: // ctrl-c
		cout.Printf("^C\n")
		return "", errors.New("interrupted")
	case '\r', '\n':
		cout.Printf("\n")
		return "", nil
	}
	cout.Printf("%c\n", b)
	return strings.ToLower(string(b)), nil
}

// Confirm prints prompt and returns true when the user presses y.
func Confirm(prompt string) (bool, error) {
	answer, err := PromptKey(prompt + " <gray>[y/N]</> ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(answer, "y"), nil
}

// Apply-loop outcomes: what happened to one candidate. Every check's apply
// loop and the milestone setter speak this vocabulary.
const (
	ApplyPreviewed = iota // dry-run: card shown, nothing changed
	ApplySet              // the mutation landed on GitHub
	ApplyFailed           // the mutation failed (reported, not fatal)
	ApplySkipped          // the human said no
	ApplyQuit             // the human quit the session
)

// AskAccept is AskClose's go-ahead — distinct from every Apply* outcome.
const AskAccept = -1

// AskClose is the per-candidate confirmation every close check shares:
// (a)ccept the close, (s)kip it, (p)review the comment that would be posted,
// (o)pen the issue, (q)uit the run. Preview and open loop back to the prompt so
// the human answers after seeing what they asked for; with no comment to show
// (the milestone setter) the preview key drops out of the offer.
func AskClose(question, comment, url string) (int, error) {
	// magenta, not lightMagenta — that one belongs to milestones and versions
	keys := "<green>(a)</>ccept <red>(s)</>kip (o)pen (q)uit"
	if comment != "" {
		keys = "<green>(a)</>ccept <red>(s)</>kip <magenta>(p)</>review (o)pen (q)uit"
	}
	for {
		ans, err := PromptKey(fmt.Sprintf("      %s %s <gray>></> ", question, keys))
		if err != nil {
			return ApplyFailed, err
		}
		switch strings.ToLower(ans) {
		case "a", "y":
			return AskAccept, nil
		case "s", "n", "":
			return ApplySkipped, nil
		case "p":
			printCommentPreview(comment)
		case "o":
			OpenInBrowser(url)
		case "q":
			return ApplyQuit, nil
		}
	}
}

// printCommentPreview shows the comment exactly as it would land on the issue.
// The body goes to the writer unparsed — close comments are markdown, and an
// angle-bracketed url in one is not a colour tag.
func printCommentPreview(comment string) {
	if comment == "" {
		return
	}
	cout.Printf("\n      <magenta>the comment that would be posted:</>\n")
	for line := range strings.SplitSeq(comment, "\n") {
		_, _ = fmt.Fprintf(cout.Writer(), "        %s\n", line)
	}
	cout.Printf("\n")
}

// OpenInBrowser opens the url, reporting rather than failing on error.
func OpenInBrowser(url string) {
	if err := browser.OpenURL(url); err != nil {
		cout.Errorf("      <yellow>WARNING:</> opening browser: %v\n", err)
	}
}
