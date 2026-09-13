package close

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// Classes by the shape of the maintainer's last word: asked means it wanted
// something back (information, a repro, confirmation) that never came; said
// means it stated a position (by design, upstream, out of scope) nobody has
// disputed since.
const (
	passStale          = "stale"
	promptStale        = "issue-stale-close"
	templateStaleClose = "stale-close"
	// the asked class closes as no-response (reusing the rules-path reason it
	// supersedes); said gets its own
	reasonStaleConcluded = "maintainer-concluded"

	classStaleWaiting = "waiting"
	classStaleAsked   = "asked"
	classStaleSaid    = "said"

	// the label maintainers apply when they are explicitly waiting on the
	// reporter — the strongest form of asked, so its window is far shorter
	labelWaitingResponse = "waiting-response"

	// how long the maintainer's last word must have hung unanswered — the
	// explicit waiting-response label needs far less benefit of the doubt
	staleQuietDays        = 365
	staleWaitingQuietDays = 90
)

var staleClassRank = map[string]int{classStaleWaiting: 2, classStaleAsked: 1, classStaleSaid: 0}

// StaleOpts configures the stale audit and its apply modes.
type StaleOpts struct {
	Link                string // waiting | asked | said ("" = all)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// staleFinding is one open issue where a maintainer had the last word over a
// year ago and nobody ever responded — the conversation is over, only the
// close is missing.
type staleFinding struct {
	issue          *db.Issue
	last           *db.Comment // the maintainer's last word
	mentionsAuthor bool        // it addressed the reporter directly (@author)
	class          string
}

// Stale finds OPEN issues whose thread ended with a maintainer speaking and
// nobody answering for over a year: a request for information that never
// came, or a conclusion (by design, upstream, out of scope) nobody disputed.
// The AI reads what the maintainer actually said — a commitment ("we'll fix
// this") is the opposite of closeable — before blessing a close as not
// planned. Question-labelled issues belong to koi close questions and are not
// touched here.
func (f *Flags) Stale(link string) error {
	o := StaleOpts{Link: link, FlagsApplyModes: f.Modes}
	if err := f.RequireAIEarly(); err != nil {
		return err
	}
	if !f.NoAutoFetch {
		if err := f.AutoFetch(); err != nil {
			return err
		}
	}

	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	col, err := f.collectStale(d, o.Link)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d of %d open issues end on a maintainer's unanswered last word:</>\n", len(findings), col.open)
	for _, c := range []struct{ class, tag, desc string }{
		{classStaleWaiting, cli.TagGreen, "labelled waiting-response and the reporter never came back"},
		{classStaleAsked, cli.TagYellow, "the maintainer asked for something that never came"},
		{classStaleSaid, cli.TagOrange, "the maintainer stated a position nobody disputed"},
	} {
		if n := col.counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-6s</> <yellow>%d</>  <gray>%s</>\n", c.tag, c.class, n, c.desc)
		}
	}
	cout.Printf("  <gray>skipped: %d where the silence is under the class's window · %s</>\n", col.recent, keepSummary(col.protected))
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyStale(d, findings, o, true)
	case o.Apply:
		return f.applyStale(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.staleJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passStale, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b staleFinding) int {
			av, bv := -1.0, -1.0
			if v := verdicts[a.issue.Number]; v != nil {
				av = v.Confidence
			}
			if v := verdicts[b.issue.Number]; v != nil {
				bv = v.Confidence
			}
			switch {
			case av > bv:
				return -1
			case av < bv:
				return 1
			default:
				return 0
			}
		})
	} else {
		cout.Printf("<gray>--ai=false: listing without scores</>\n")
	}

	for n := range findings {
		f.printStaleCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close stale --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// staleCollection is everything collectStale learns in one scan.
type staleCollection struct {
	findings  []staleFinding
	counts    map[string]int
	open      int // open issues in the db
	recent    int // maintainer has the last word, but not for a year yet
	protected map[string]int
}

// collectStale walks every open issue looking for threads that end on a
// maintainer's comment (bots aside) left unanswered for over a year. The last
// word must not be the reporter's own — a maintainer following up on their own
// issue is just the reporter talking.
func (f *Flags) collectStale(d *db.DB, link string) (*staleCollection, error) {
	col := &staleCollection{counts: map[string]int{}, protected: map[string]int{}}
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	col.open = len(issues)
	now := time.Now()

	cout.Printf("scanning <yellow>%d</> open issues for maintainer last words left unanswered...\n", len(issues))
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, serr
		}
		if s == nil {
			s = &db.Signals{IssueNumber: i.Number}
		}
		// question threads ending on a maintainer reply are koi close questions'
		// answered class — one close path per issue
		if s.Kind == signalKindQuestion {
			continue
		}

		comments, cerr := d.CommentsFor(i.Number)
		if cerr != nil {
			return nil, cerr
		}
		var last *db.Comment
		for ci := range slices.Backward(comments) {
			if !questionsBot(comments[ci].Author) {
				last = &comments[ci]
				break
			}
		}
		if last == nil || !last.IsMaintainer() || last.Author == i.Author {
			continue
		}

		switch {
		case s.OpenLinkedPRs > 0:
			col.protected["open-pr"]++
			continue
		case i.ThumbsUp >= f.KeepReactions:
			col.protected["high-engagement"]++
			continue
		}
		// the explicit waiting-response label is the strongest asked there is,
		// so it earns the short window; without it a year of silence is needed
		quiet := staleQuietDays * 24 * time.Hour
		if i.HasLabel(labelWaitingResponse) {
			quiet = staleWaitingQuietDays * 24 * time.Hour
		}
		if now.Sub(last.CreatedAt) < quiet {
			col.recent++
			continue
		}

		fdg := staleFinding{
			issue: i, last: last, class: classStaleSaid,
			mentionsAuthor: strings.Contains(last.Body, "@"+i.Author),
		}
		switch {
		case i.HasLabel(labelWaitingResponse):
			fdg.class = classStaleWaiting
		case strings.Contains(last.Body, "?"):
			fdg.class = classStaleAsked
		}
		if link != "" && fdg.class != link {
			continue
		}
		col.findings = append(col.findings, fdg)
		col.counts[fdg.class]++
	}

	slices.SortStableFunc(col.findings, func(a, b staleFinding) int {
		if d := staleClassRank[b.class] - staleClassRank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return col, nil
}

// applyStale is both apply modes on the shared harness: plain --apply closes
// everything listed; --apply-with-ai[-auto] gates each close on the judge
// reading what the maintainer actually said, and is the recommended path — a
// "we'll fix this" last word must never robo-close.
func (f *Flags) applyStale(d *db.DB, findings []staleFinding, o StaleOpts, withAI bool) error {
	byNumber := map[int]*staleFinding{}
	numbers := make([]int, len(findings))
	for i := range findings {
		byNumber[findings[i].issue.Number] = &findings[i]
		numbers[i] = findings[i].issue.Number
	}

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}
	throttle := cli.NewThrottle()

	p := f.NewApplyPass(o.FlagsApplyModes,
		func(n int) string { return byNumber[n].issue.Title },
		func(n int, v *issue.Verdict, pos, total int, interactive bool) (int, error) {
			return f.closeOneStale(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "issues ending on a maintainer's unanswered last word"
	p.GateLabel = "concluded"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as not planned in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.staleJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passStale, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneStale handles one candidate: card, the stale-close comment citing
// the maintainer's last word, and the close as not planned (or preview under
// dry-run, or the a/s ask when interactive).
func (f *Flags) closeOneStale(d *db.DB, repo gh.Repo, fdg *staleFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printStaleCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	// waiting and asked are both the no-response close; said concluded
	reason := reasonStaleConcluded
	if fdg.class != classStaleSaid {
		reason = issue.ReasonNoResponse
	}

	comment, err := f.renderStaleComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateStaleClose, issue.StateNotPlanned)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as its thread ended?", fdg.issue.Number), comment, fdg.issue.URL)
		if perr != nil || res != issue.AskAccept {
			return res, perr
		}
	}

	throttle()
	live, err := repo.GetIssue(fdg.issue.Number)
	if err != nil {
		cout.Errorf("      <red>fetching live state: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	if live.State != cli.RESTStateOpen {
		cout.Printf("      <gray>already closed on github — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	throttle()
	if err := repo.CreateComment(fdg.issue.Number, comment); err != nil {
		cout.Errorf("      <red>comment failed: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	throttle()
	if err := repo.CloseIssue(fdg.issue.Number, issue.StateNotPlanned); err != nil {
		cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", issue.StateNotPlanned)
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reason)

	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reason,
		StateReason: issue.StateNotPlanned, Template: templateStaleClose,
		Evidence: map[string]string{
			evidenceKeyClass: fdg.class,
			"last-word":      text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.last.Body)), 120),
			"author":         fdg.last.Author, "comment-url": fdg.last.URL,
		},
		Source:         passStale,
		IssueUpdatedAt: fdg.issue.UpdatedAt,
	}
	if v != nil {
		a.Confidence = v.Confidence
		a.Evidence[evidenceKeyAI] = v.Reason
	}
	if _, err := d.ProposeAction(a); err != nil {
		return issue.ApplyFailed, err
	}
	row, err := d.GetAction(fdg.issue.Number)
	if err != nil || row == nil {
		return issue.ApplyFailed, err
	}
	if row.Status == db.StatusProposed {
		if err := d.DecideAction(row.ID, db.StatusApproved, f.Decider()); err != nil {
			return issue.ApplyFailed, err
		}
	}
	return issue.ApplySet, d.MarkApplied(row.ID, db.StatusApplied, "")
}

// renderStaleComment renders the close comment citing the maintainer's last
// word with a deep link.
func (f *Flags) renderStaleComment(fdg *staleFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateStaleClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateStaleClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateStaleClose, err)
	}
	data := struct {
		Asked        bool
		Author       string
		URL          string
		CurrentMajor int
	}{fdg.class != classStaleSaid, fdg.last.Author, fdg.last.URL, f.CurrentMajor}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateStaleClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// staleJudgeItems renders one judge block per finding: the issue, its class,
// the maintainer's last word in full, and the thread digest so the AI can see
// what led up to it — and above all what the last word actually was.
func (f *Flags) staleJudgeItems(d *db.DB, findings []staleFinding) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptStale)
	if err != nil {
		return "", nil, err
	}

	items := make([]issue.JudgeItem, 0, len(findings))
	for i := range findings {
		fdg := &findings[i]
		comments, cerr := d.CommentsFor(fdg.issue.Number)
		if cerr != nil {
			return "", nil, cerr
		}

		var b strings.Builder
		fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.issue.Number, text.OneLine(fdg.issue.Title))
		fmt.Fprintf(&b, "reported by @%s, opened %s, last activity %s\n",
			fdg.issue.Author, fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "CLASS: %s\n", strings.ToUpper(fdg.class))
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		fmt.Fprintf(&b, "THE MAINTAINER'S LAST WORD, unanswered since, by %s (%s) on %s:\n%s\n",
			fdg.last.Author, fdg.last.AuthorAssociation, fdg.last.CreatedAt.Format("2006-01-02"),
			text.TruncateRunes(issue.CleanBody(fdg.last.Body), 1500))
		if picked := issue.DigestComments(comments, 10); len(picked) > 0 {
			fmt.Fprintf(&b, "THREAD (%d of %d comments):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printStaleCard is one candidate: the issue and the maintainer's last word
// with how long it has hung, and the AI's score when judged.
func (f *Flags) printStaleCard(fdg *staleFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	now := time.Now()
	verb := "said"
	if fdg.class != classStaleSaid {
		verb = "asked"
	}
	assocTag, assocName := assocDisplay(fdg.last.AuthorAssociation)
	who := fmt.Sprintf("<%s>%s</>", assocTag, fdg.last.Author)
	if assocName != "" {
		who += fmt.Sprintf(" <gray>(%s)</>", assocName)
	}
	at := ""
	if fdg.class == classStaleWaiting {
		at = " <gray>· labelled</> <lightYellow>waiting-response</>"
	}
	if fdg.mentionsAuthor {
		at += fmt.Sprintf(" <gray>· addressed</> @%s <gray>directly</>", fdg.issue.Author)
	}
	cout.Printf("      %s <gray>%s %s ago, unanswered since</>%s<gray>:</>\n", who, verb, text.HumanAge(fdg.last.CreatedAt, now), at)
	cout.Printf("      <gray>“</>%s<gray>”</> <darkGray>%s</>\n",
		text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.last.Body)), 160), fdg.last.URL)
	cli.PrintVerdict(v)
}
