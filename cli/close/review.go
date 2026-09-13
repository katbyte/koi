package close

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

const (
	passReview   = "close-review"
	promptReview = "issue-close-disputed"

	// disputing comments ARE the evidence here, so they get more room than the
	// digest budget the checks use for thread colour
	reviewCommentRunes = 800

	// classes: ledger = an applied close action (koi did it), manual = a
	// closed issue with the closer's own comment at close time but no action
	// row (closed by hand outside koi) — --exhaustive sweeps those in
	reviewClassLedger = "ledger"
	reviewClassManual = "manual"

	// a closer comment this close to the close is part of the closing act:
	// before it, it identifies who closed; after it, it is not a dispute
	reviewCloseSlop = 5 * time.Minute
	// how far before the close a closer comment still reads as the closing
	// comment (comment-then-close sometimes happens the same sitting, not
	// the same second)
	reviewCloseWindow = 24 * time.Hour
)

// reviewFinding is one closed issue with comments left after the close.
// action is nil for manual (--exhaustive) findings; closeComment is the
// closer's close-time comment that qualified a manual finding.
type reviewFinding struct {
	number       int
	title        string
	closedAt     time.Time
	stateReason  string
	class        string
	action       *db.Action  // ledger findings only
	closeComment *db.Comment // manual findings only
	comments     []db.Comment
}

// reLastWindow is the --last shorthand: <n><d|w|m|y>, e.g. 10w or 2m.
var reLastWindow = regexp.MustCompile(`^(\d+)([dwmy])$`)

// parseLastWindow turns --last shorthand into a duration (months are 30 days,
// years 365 — close enough for a comment window).
func parseLastWindow(s string) (time.Duration, error) {
	m := reLastWindow.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("bad --last %q: use <n>d, <n>w, <n>m, or <n>y — e.g. 10w or 2m", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad --last %q: the count must be a positive number", s)
	}
	day := 24 * time.Hour
	switch m[2] {
	case "d":
		return time.Duration(n) * day, nil
	case "w":
		return time.Duration(n) * 7 * day, nil
	case "m":
		return time.Duration(n) * 30 * day, nil
	default:
		return time.Duration(n) * 365 * day, nil
	}
}

// CloseReview scans the issues we closed for comments left AFTER the close and
// has the AI judge whether they dispute the close enough that reopening is the
// right call. Everything reads the local db — the incremental sync refetches
// closed issues on new activity (no is:open filter), so the comments are
// already here. The base list is the applied close actions; --exhaustive adds
// every closed issue with the closer's own comment at close time (closes done
// by hand, outside koi). --last narrows to comments in a recent window; the
// apply modes reopen.
func (f *Flags) CloseReview() error {
	var cutoff time.Time
	windowNote := ""
	if f.Cmd.ReviewLast != "" {
		dur, err := parseLastWindow(f.Cmd.ReviewLast)
		if err != nil {
			return err
		}
		cutoff = time.Now().Add(-dur)
		windowNote = fmt.Sprintf(" <gray>(comments in the last %s)</>", f.Cmd.ReviewLast)
	}

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

	findings, reopened, counts, err := f.collectCloseReview(d, cutoff)
	if err != nil {
		return err
	}
	if counts[reviewClassLedger]+counts[reviewClassManual] == 0 {
		cout.Printf("no closes to review — nothing has been closed from this db\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d closed issues have new comments since their close</>%s\n",
		len(findings), counts[reviewClassLedger]+counts[reviewClassManual], windowNote)
	cout.Printf("  <lightBlue>%-8s</> <yellow>%d</> <gray>of %d close actions</>\n",
		reviewClassLedger, countClass(findings, reviewClassLedger), counts[reviewClassLedger])
	if f.Cmd.ReviewExhaustive {
		cout.Printf("  <fg=208>%-8s</> <yellow>%d</> <gray>of %d closed issues %s commented on at close (no action row)</>\n",
			reviewClassManual, countClass(findings, reviewClassManual), counts[reviewClassManual], f.Cmd.ReviewCloser)
	}
	if len(reopened) > 0 {
		cout.Printf("  <gray>%d close(s) have since been reopened — koi or by hand; the review report lists them</>\n", len(reopened))
	}
	if len(findings) == 0 {
		return nil
	}

	o := f.Modes
	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyCloseReview(d, findings, true)
	case o.Apply:
		return f.applyCloseReview(d, findings, false)
	}

	// report: score every finding (cached verdicts reuse until new comments
	// change the block) and list surest-disputed first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.reviewJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passReview, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b reviewFinding) int {
			av, bv := -1.0, -1.0
			if v := verdicts[a.number]; v != nil {
				av = v.Confidence
			}
			if v := verdicts[b.number]; v != nil {
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
		cout.Printf("<gray>--ai=false: listing without dispute scores, newest close first</>\n")
		slices.SortStableFunc(findings, func(a, b reviewFinding) int { return b.closedAt.Compare(a.closedAt) })
	}

	for n := range findings {
		f.printReviewCard(&findings[n], n+1, len(findings), verdicts[findings[n].number])
	}
	cout.Printf("\nnext: <cyan>koi close review --apply-with-ai</> to confirm each reopen, <cyan>--apply-with-ai-auto</> to trust the scores, or <cyan>koi reopen #</> for a single issue\n")
	return nil
}

// countClass tallies the findings in one class.
func countClass(findings []reviewFinding, class string) int {
	n := 0
	for i := range findings {
		if findings[i].class == class {
			n++
		}
	}
	return n
}

// collectCloseReview builds two lists from the local db. findings: closes
// with comments created after the close (and inside the --last window when
// set) — the disputes to weigh; the closer's own comments within minutes of
// the close are part of the closing act and never count. reopened: closes
// whose issue is OPEN again — however it was reopened, koi or by hand on
// GitHub (the sync flips the state) — with the same post-close comments as
// the story of why. The base list is the close actions; --exhaustive adds
// closed issues the closer commented on at close time without an action row.
func (f *Flags) collectCloseReview(d *db.DB, cutoff time.Time) (findings, reopened []reviewFinding, counts map[string]int, err error) {
	actions, err := d.Actions(db.ActionFilter{Action: db.ActionClose})
	if err != nil {
		return nil, nil, nil, err
	}
	titles, err := d.IssueTitles()
	if err != nil {
		return nil, nil, nil, err
	}
	states, err := d.IssueStates()
	if err != nil {
		return nil, nil, nil, err
	}
	closer := f.Cmd.ReviewCloser

	counts = map[string]int{}
	newSince := func(number int, closedAt time.Time) ([]db.Comment, error) {
		comments, cerr := d.CommentsFor(number)
		if cerr != nil {
			return nil, cerr
		}
		var kept []db.Comment
		for _, c := range comments {
			if !c.CreatedAt.After(closedAt) || (!cutoff.IsZero() && !c.CreatedAt.After(cutoff)) {
				continue
			}
			if c.Author == closer && c.CreatedAt.Before(closedAt.Add(reviewCloseSlop)) {
				continue // the closing comment, when it landed just after the close
			}
			kept = append(kept, c)
		}
		return kept, nil
	}

	ledger := map[int]bool{}
	for _, a := range actions {
		if a.Status != db.StatusApplied && a.Status != db.StatusReopened {
			continue // proposed/failed/stale rows never closed anything
		}
		ledger[a.IssueNumber] = true
		counts[reviewClassLedger]++
		closedAt := a.AppliedAt
		// a reopened row's applied_at is the reopen time (MarkApplied stamps
		// now) — the decision time is the close for comment filtering
		if a.Status == db.StatusReopened || closedAt.IsZero() {
			closedAt = a.DecidedAt
		}
		if closedAt.IsZero() {
			closedAt = a.AppliedAt
		}
		kept, cerr := newSince(a.IssueNumber, closedAt)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		fdg := reviewFinding{
			number: a.IssueNumber, title: titles[a.IssueNumber], closedAt: closedAt,
			stateReason: a.StateReason, class: reviewClassLedger, action: a, comments: kept,
		}
		// open again = it WAS reopened, by whoever, however — koi's own record
		// only covers reopens koi performed, the state covers them all
		if states[a.IssueNumber] == db.IssueOpen || a.Status == db.StatusReopened {
			reopened = append(reopened, fdg)
			continue
		}
		if len(kept) > 0 {
			findings = append(findings, fdg)
		}
	}

	if !f.Cmd.ReviewExhaustive {
		return findings, reopened, counts, nil
	}

	closed, err := d.ClosedIssues()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, i := range closed {
		if ledger[i.Number] || i.ClosedAt.IsZero() {
			continue
		}
		comments, cerr := d.CommentsFor(i.Number)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		// the closer's comment nearest the close, inside the closing window —
		// comment-then-close is the pattern for both koi and by-hand closes
		var closeComment *db.Comment
		for n := range comments {
			c := &comments[n]
			if c.Author == closer &&
				c.CreatedAt.After(i.ClosedAt.Add(-reviewCloseWindow)) && c.CreatedAt.Before(i.ClosedAt.Add(reviewCloseSlop)) {
				closeComment = c
			}
		}
		if closeComment == nil {
			continue
		}
		counts[reviewClassManual]++
		kept, cerr := newSince(i.Number, i.ClosedAt)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		if len(kept) == 0 {
			continue
		}
		findings = append(findings, reviewFinding{
			number: i.Number, title: i.Title, closedAt: i.ClosedAt,
			stateReason: strings.ToLower(i.StateReason), class: reviewClassManual,
			closeComment: closeComment, comments: kept,
		})
	}
	return findings, reopened, counts, nil
}

// applyCloseReview is both apply modes on the shared harness, reworded around
// reopening: plain --apply reopens everything listed (any new comment — exists
// for pattern consistency); --apply-with-ai[-auto] gates each reopen on the
// dispute score and is the recommended path.
func (f *Flags) applyCloseReview(d *db.DB, findings []reviewFinding, withAI bool) error {
	repo, err := f.NewRepo()
	if err != nil {
		return err
	}

	byNumber := map[int]*reviewFinding{}
	numbers := make([]int, len(findings))
	for i := range findings {
		byNumber[findings[i].number] = &findings[i]
		numbers[i] = findings[i].number
	}

	throttle := cli.NewThrottle()
	p := f.NewApplyPass(f.Modes,
		func(n int) string { return byNumber[n].title },
		func(n int, v *issue.Verdict, pos, total int, interactive bool) (int, error) {
			return f.reopenOneReview(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "closed issues with new comments"
	p.GateLabel = "dispute"
	p.One, p.Verb, p.Done = "reopen", "reopening", "reopened"
	p.ConfirmAll = fmt.Sprintf("reopen up to <yellow>%d</> issues in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("reopen issues the AI scores ≥ <green>%.2f</> disputed (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.reviewJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passReview, promptText, items, onReady, onBatch)
		return jerr
	})
}

// reopenOneReview handles one candidate: card, then the reopen itself (or a
// preview under dry-run, or the a/s ask when interactive). No comment is
// posted — the reopened issue's thread already carries the dispute.
func (f *Flags) reopenOneReview(d *db.DB, repo gh.Repo, fdg *reviewFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printReviewCard(fdg, pos, total, v)

	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would reopen</>\n")
		return issue.ApplyPreviewed, nil
	}
	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("reopen <cyan>#%d</>?", fdg.number), "", f.IssueURL(fdg.number))
		if perr != nil || res != issue.AskAccept {
			return res, perr
		}
	}

	throttle()
	live, err := repo.GetIssue(fdg.number)
	if err != nil {
		cout.Errorf("      <red>fetching live state: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	if live.State == cli.RESTStateOpen {
		cout.Printf("      <gray>already open on github — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	throttle()
	if err := repo.ReopenIssue(fdg.number); err != nil {
		cout.Errorf("      <red>reopen failed: %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	if fdg.action != nil {
		if err := d.MarkApplied(fdg.action.ID, db.StatusReopened, ""); err != nil {
			return issue.ApplyFailed, err
		}
	}
	cout.Printf("      <fg=28>reopened</>\n")
	cout.QuietOnlyf("%d@reopened@%s\n", fdg.number, fdg.class)
	return issue.ApplySet, nil
}

// reviewJudgeItems renders one judge block per finding: what the issue was,
// why and how it was closed, and every comment left since — so the AI can
// tell a genuine dispute from thanks, bot noise, or a different problem.
func (f *Flags) reviewJudgeItems(d *db.DB, findings []reviewFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptReview)
	if err != nil {
		return "", nil, err
	}

	items := make([]issue.JudgeItem, 0, len(findings))
	for i := range findings {
		fdg := &findings[i]

		var b strings.Builder
		fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.number, text.OneLine(fdg.title))
		if a := fdg.action; a != nil {
			fmt.Fprintf(&b, "CLOSED %s as %s, reason code: %s\n", fdg.closedAt.Format("2006-01-02"), a.StateReason, a.Reason)
			for _, k := range text.SortedKeys(a.Evidence) {
				if k == evidenceKeyAI || a.Evidence[k] == "" {
					continue
				}
				fmt.Fprintf(&b, "CLOSE EVIDENCE %s: %s\n", k, text.TruncateRunes(text.OneLine(a.Evidence[k]), 300))
			}
			if r := a.Evidence[evidenceKeyAI]; r != "" {
				fmt.Fprintf(&b, "AI REASONING AT CLOSE: %s\n", text.OneLine(r))
			}
		} else {
			fmt.Fprintf(&b, "CLOSED %s as %s, by hand by the maintainer (%s)\n",
				fdg.closedAt.Format("2006-01-02"), cli.OrDash(fdg.stateReason), f.Cmd.ReviewCloser)
			fmt.Fprintf(&b, "CLOSING COMMENT: %s\n", text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.closeComment.Body)), reviewCommentRunes))
		}
		if is, gerr := d.GetIssue(fdg.number); gerr == nil && is != nil {
			fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(is.Body), cli.IssueBodyRunes))
		}
		b.WriteString("NEW COMMENTS SINCE THE CLOSE:\n")
		for _, c := range fdg.comments {
			fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
				text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), reviewCommentRunes))
		}
		items = append(items, issue.JudgeItem{Number: fdg.number, Block: b.String()})
	}
	return promptText, items, nil
}

// writeReviewReport renders review-<stamp>.html in out: the close review's
// current findings (disputed) and every close since reopened (reopened).
// Returns an empty path when there is nothing to show. Written by koi close
// report on every run.
func (f *Flags) writeReviewReport(d *db.DB, o cli.FlagsReport, now time.Time) (string, *cli.ReportData, error) {
	findings, reopenedFindings, _, err := f.collectCloseReview(d, time.Time{})
	if err != nil {
		return "", nil, err
	}
	disputed, err := f.disputedReportSection(d, o, now, findings)
	if err != nil {
		return "", nil, err
	}
	reopened := f.reopenedReportSection(reopenedFindings, now)
	if disputed.Total+reopened.Total == 0 {
		return "", nil, nil
	}

	data := cli.ReportData{
		Repo: f.GH.Repo, Noun: "review findings", WithAI: o.WithAI,
		GeneratedAt: now.Format("2006-01-02 15:04"),
		Sections:    []cli.ReportSection{disputed, reopened},
		Total:       disputed.Total + reopened.Total,
	}
	if err := os.MkdirAll(o.Out, 0o750); err != nil {
		return "", nil, fmt.Errorf("creating %s: %w", o.Out, err)
	}
	path := filepath.Join(o.Out, cli.ReportFileName("review", now))
	if err := cli.WriteReportHTML(path, &data); err != nil {
		return "", nil, err
	}
	return path, &data, nil
}

// disputedReportSection builds the "someone commented after the close" section
// from the same collection koi close review runs on.
func (f *Flags) disputedReportSection(d *db.DB, o cli.FlagsReport, now time.Time, findings []reviewFinding) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     "review-disputed",
		Name:     "close review · disputed",
		Question: "someone commented after we closed it — do they dispute the close?",
		Description: "Every still-closed issue with comments left AFTER its close, from the local db (the sync " +
			"refetches closed issues on new activity). Base list = the close actions; with --exhaustive, closed issues " +
			"the closer commented on at close time with no action row (by-hand closes) join as class manual. The AI " +
			"judges whether the new comments genuinely dispute the close — the author saying it is not fixed, a fresh " +
			"reproduction on a current version, a maintainer objecting — versus thanks, bot notes, or a different " +
			"problem. Issues open again sit in the reopened section instead. Applying reopens, posting no comment.",
		Command: "koi close review [--exhaustive] [--last 10w] --apply-with-ai / --apply-with-ai-auto",
	}
	s.Total = len(findings)
	var err error
	s.Classes = []cli.ReportClass{
		{Name: reviewClassLedger, Count: countClass(findings, reviewClassLedger), Kind: cli.KindMid},
		{Name: reviewClassManual, Count: countClass(findings, reviewClassManual), Kind: cli.KindWarn},
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.reviewJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passReview, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *reviewFinding) int { return x.number }, verdicts)
	}

	for i := range findings {
		item := f.reviewReportItem(&findings[i], now)
		cli.AttachVerdict(&item, verdicts[findings[i].number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// reopenedReportSection lists every close whose issue is open again — however
// it was reopened, koi or by hand on GitHub — with the post-close comments
// that tell the story of why.
func (f *Flags) reopenedReportSection(reopened []reviewFinding, now time.Time) cli.ReportSection {
	s := cli.ReportSection{
		Slug:     "review-reopened",
		Name:     "close review · reopened",
		Question: "which of our closes were reopened?",
		Description: "Every close whose issue is OPEN again — reopened by the review's apply modes, koi reopen, or by " +
			"hand on GitHub (the sync flips the state either way; koi's own reopen record also counts before the sync " +
			"catches up). The comments left after the close are shown — usually the dispute that earned the reopen.",
		Command: "koi close review --apply-with-ai · koi reopen #",
	}
	s.Total = len(reopened)
	reasons := map[string]int{}
	for i := range reopened {
		if a := reopened[i].action; a != nil {
			reasons[a.Reason]++
		}
		s.Items = append(s.Items, f.reviewReportItem(&reopened[i], now))
	}
	for _, r := range text.SortedKeys(reasons) {
		s.Classes = append(s.Classes, cli.ReportClass{Name: r, Count: reasons[r], Kind: cli.KindWarn})
	}
	return s
}

// reviewReportItem renders one finding for the review report: how the close
// happened and every comment left since, linked and quoted.
func (f *Flags) reviewReportItem(fdg *reviewFinding, now time.Time) cli.ReportItem {
	item := cli.ReportItem{
		Number: fdg.number, URL: f.issHTMLURL(fdg.number), Title: text.OneLine(fdg.title),
		Meta: fmt.Sprintf("closed %s ago as %s · %d comment(s) since", text.HumanAge(fdg.closedAt, now),
			cli.OrDash(fdg.stateReason), len(fdg.comments)),
	}
	closedAs := []cli.ReportSpan{cli.Span("closed "+fdg.closedAt.Format("2006-01-02"), cli.KindDim)}
	if a := fdg.action; a != nil {
		closedAs = append(closedAs, cli.Span(a.Reason, cli.KindMid))
		if a.Confidence > 0 {
			closedAs = append(closedAs, cli.Span(fmt.Sprintf("AI %.2f at close", a.Confidence), cli.KindDim))
		}
		if a.Status == db.StatusReopened {
			closedAs = append(closedAs, cli.Span("reopened via koi", cli.KindWarn))
		}
	} else {
		closedAs = append(closedAs,
			cli.Span(reviewClassManual, cli.KindWarn),
			cli.LinkSpan("closing comment by "+f.Cmd.ReviewCloser, fdg.closeComment.URL))
	}
	item.Evidence = append(item.Evidence, closedAs)
	for n, c := range fdg.comments {
		if n == 8 {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span(fmt.Sprintf("… and %d more comments", len(fdg.comments)-n), cli.KindDim),
			})
			break
		}
		item.Evidence = append(item.Evidence, []cli.ReportSpan{
			cli.LinkSpan(fmt.Sprintf("[%s] %s (%s)", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation), c.URL),
			cli.Span(text.TruncateRunes(text.OneLine(c.Body), 240), cli.KindQuote),
		})
	}
	return item
}

// printReviewCard is one closed issue: how and why it was closed, every
// comment left since, and the AI's dispute score when judged.
func (f *Flags) printReviewCard(fdg *reviewFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.number, text.TruncateRunes(text.OneLine(fdg.title), 90), f.IssueURL(fdg.number))

	line := fmt.Sprintf("      <gray>closed</> %s <gray>as</> <lightMagenta>%s</>",
		fdg.closedAt.Format("2006-01-02"), cli.OrDash(fdg.stateReason))
	if a := fdg.action; a != nil {
		line += fmt.Sprintf(" <gray>·</> <lightBlue>%s</>", a.Reason)
		if a.Confidence > 0 {
			line += fmt.Sprintf(" <gray>· AI %.2f at close</>", a.Confidence)
		}
	} else {
		line += fmt.Sprintf(" <gray>·</> <fg=208>%s</> <gray>· by %s, no action row</>", reviewClassManual, f.Cmd.ReviewCloser)
	}
	cout.Printf("%s\n", line)

	shown := 0
	for _, c := range fdg.comments {
		if shown == 6 {
			cout.Printf("      <gray>… and %d more comments</>\n", len(fdg.comments)-shown)
			break
		}
		shown++
		cout.Printf("      <gray>[%s]</> <white>%s</> <gray>(%s):</> %s\n",
			c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
			text.TruncateRunes(text.OneLine(c.Body), 140))
	}
	cli.PrintVerdict(v)
}
