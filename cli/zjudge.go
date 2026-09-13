// The shared AI-judging plumbing every check rides: the thin wrapper over
// lib/issue's judge engine, prompt preparation, score colouring, and the
// full-text fetcher that feeds judge blocks.

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// JudgeBlocks runs the shared judge (issue.Judge) configured from the flags.
// The model canonicalises inside NewJudge; it is copied back so every later
// display matches the identity verdicts are cached under.
func (f *FlagData) JudgeBlocks(d *db.DB, pass, promptText string, items []issue.JudgeItem,
	onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error),
) (map[int]*issue.Verdict, error) {
	return f.JudgeBlocksBatch(d, pass, promptText, 0, items, onReady, onBatch)
}

// JudgeBlocksBatch is JudgeBlocks with a smaller batch for passes whose blocks
// are huge — docs ships page content, so fewer pairings per call buys each one
// more room. batch <= 0 keeps the judge's default.
func (f *FlagData) JudgeBlocksBatch(d *db.DB, pass, promptText string, batch int, items []issue.JudgeItem,
	onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error),
) (map[int]*issue.Verdict, error) {
	if err := f.RequireAI(); err != nil {
		return nil, err
	}
	j := issue.NewJudge(d, f.AI.Cmd, f.AI.Model, time.Duration(f.AI.TimeoutMinutes)*time.Minute)
	if batch > 0 {
		j.BatchSize = batch
	}
	f.AI.Model = j.Model()
	return j.Blocks(pass, promptText, items, onReady, onBatch)
}

// FetchTexts fills the texts cache for every wanted number not yet cached, 25
// per aliased query. Numbers that no longer resolve are cached empty so they
// aren't refetched forever.
func (f *FlagData) FetchTexts(d *db.DB, want []int) error {
	cached, err := d.Texts()
	if err != nil {
		return err
	}
	var need []int
	for _, n := range want {
		if t, ok := cached[n]; !ok || !t.HasTail {
			need = append(need, n) // pre-tail rows refetch once to pick up the comments
		}
	}
	if len(need) == 0 {
		return nil
	}
	if f.NoAutoFetch {
		cout.Printf("<yellow>%d issue/PR texts not in the local cache and --no-auto-fetch is set — judging with what's cached</>\n", len(need))
		return nil
	}

	owner, name, err := f.RepoOwnerName()
	if err != nil {
		return err
	}
	client := f.NewGraphQL()
	cout.Printf("fetching full text for <yellow>%d</> issues/PRs from <white>%s</>/<cyan>%s</>...\n", len(need), owner, name)

	for start := 0; start < len(need); start += 25 {
		batch := need[start:min(start+25, len(need))]
		nodes, rl, err := client.Texts(owner, name, batch)
		if err != nil {
			return err
		}
		texts := make([]db.Text, 0, len(batch))
		for _, n := range batch {
			node := nodes[n]
			if node == nil {
				texts = append(texts, db.Text{Number: n})
				continue
			}
			// full timestamps so splitTailAt can place same-day comments on the
			// right side of a close; a leading note marks truncated discussions
			var tail strings.Builder
			if hidden := node.Comments.TotalCount - len(node.Comments.Nodes); hidden > 0 {
				fmt.Fprintf(&tail, "(%d earlier comments not shown)\n", hidden)
			}
			for _, c := range node.Comments.Nodes {
				fmt.Fprintf(&tail, "[%s] %s: %s\n", c.CreatedAt, c.Author.Login, text.TruncateRunes(text.OneLine(c.Body), CommentRunes))
			}
			texts = append(texts, db.Text{
				Number: n, IsPR: node.Typename == TypePullRequest,
				State: node.State, Title: node.Title, Body: node.Body, Tail: tail.String(),
			})
		}
		if err := d.SaveTexts(texts); err != nil {
			return err
		}
		cout.Printf("  <gray>%d/%d fetched · rate limit: %d remaining</>\n", start+len(batch), len(need), rl.Remaining)
		rl.WaitIfLow()
	}
	return nil
}

// PreparePrompt loads a prompt template and substitutes the version
// placeholders, so a prompt can talk about "majors 1 to 3" without the
// current release being baked into the file.
func (f *FlagData) PreparePrompt(name string) (string, error) {
	p, err := assets.Prompt(name)
	if err != nil {
		return "", err
	}
	recent := fmt.Sprintf("%d or %d", f.CurrentMajor-1, f.CurrentMajor)
	p = strings.ReplaceAll(p, "{{CURRENT_MAJOR}}", strconv.Itoa(f.CurrentMajor))
	p = strings.ReplaceAll(p, "{{LEGACY_MAX}}", strconv.Itoa(f.CurrentMajor-2))
	p = strings.ReplaceAll(p, "{{RECENT_MAJORS}}", recent)
	return p, nil
}

const (
	// JudgeThreshold is the minimum AI confidence for an apply mode to act;
	// below it the finding is reported and skipped. Also --apply-with-ai-auto's
	// bare-flag default.
	JudgeThreshold = 0.7

	// judge-block budgets: how much of an issue body, an evidence PR body, and
	// a single comment go in — accuracy beats cost, so the slices are generous.
	IssueBodyRunes = 3000
	PRBodyRunes    = 2000
	CommentRunes   = 400

	// TypePullRequest is GraphQL's __typename for PRs on issue-or-PR fields.
	TypePullRequest = "PullRequest"

	// shared colour tag names for the class/score/state tag helpers.
	TagGreen     = "green"
	TagYellow    = "yellow"
	TagOrange    = "fg=208"
	TagRed       = "red"
	TagLightBlue = "lightBlue"
	TagGray      = "gray"
)

// PrintVerdict prints the AI's score and reason for a judged finding.
func PrintVerdict(v *issue.Verdict) {
	if v == nil {
		return
	}
	cout.Printf("\n      <gray>AI:</> <%s>%.2f</>\n", ScoreTag(v.Confidence), v.Confidence)
	cout.Printf("        <lightWhite>%s</>\n", text.OneLine(v.Reason))
}

// ScoreTag colours a match confidence: green at or above the apply threshold,
// orange in the murky middle, red for a clear non-match.
func ScoreTag(confidence float64) string {
	switch {
	case confidence >= JudgeThreshold:
		return TagGreen
	case confidence >= 0.4:
		return TagOrange
	default:
		return TagRed
	}
}

// Column names shared by every csv this tool writes.
const (
	CSVColNumber = "number"
	CSVColTitle  = "title"
	CSVColURL    = "url"
)

// LinkCited marks a milestone determined from a changelog bullet citing the
// issue number directly (between linked and mention in strength).
const LinkCited = "cited"

// ClassTag is the one colour per evidence class, strongest to weakest: green
// closed-by, lightBlue linked, yellow cited, orange mention. lightMagenta is
// reserved for milestones/versions — cited must not blend into the version
// printed beside it.
func ClassTag(class string) string {
	switch class {
	case db.LinkClosedBy:
		return TagGreen
	case db.LinkLinked:
		return TagLightBlue
	case LinkCited:
		return TagYellow
	default:
		return TagOrange
	}
}

// OrDash renders an empty string as an em-dash for tabular output.
func OrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
