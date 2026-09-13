// Shared apply plumbing for every label family: the ask, the live-open
// guard, and the label add itself.

package label

import (
	"fmt"
	"strings"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
)

// labelVerbs sets the pass wording every label family shares.
func labelVerbs(p *issue.ApplyPass) {
	p.One, p.Verb, p.Done = "label", "labelling", "labelled"
}

// addLabels finishes one candidate the caller has already carded: the a/s ask
// when interactive, the live-open guard, and the add.
func (f *Flags) addLabels(repo gh.Repo, i *db.Issue, labels []string, throttle func(), ask bool) (int, error) {
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would add %s</>\n", strings.Join(labels, " "))
		return issue.ApplyPreviewed, nil
	}
	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("label <cyan>#%d</> with <lightMagenta>%s</>?", i.Number, strings.Join(labels, " ")), "", i.URL)
		if perr != nil || res != issue.AskAccept {
			return res, perr
		}
	}

	throttle()
	live, err := repo.GetIssue(i.Number)
	if err != nil {
		cout.Errorf("      <red>fetching live state: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	if live.State != cli.RESTStateOpen {
		cout.Printf("      <gray>already closed on github — skipped</>\n")
		return issue.ApplySkipped, nil
	}
	throttle()
	if err := repo.AddLabels(i.Number, labels); err != nil {
		cout.Errorf("      <red>labelling failed: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	cout.Printf("      <fg=28>added</> <lightMagenta>%s</>\n", strings.Join(labels, " "))
	cout.QuietOnlyf("%d@labelled@%s\n", i.Number, strings.Join(labels, ","))
	return issue.ApplySet, nil
}
