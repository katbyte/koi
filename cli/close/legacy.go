package close

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

const (
	passLegacy   = "legacy"
	promptLegacy = "legacy-bug-close"

	// signal kinds the legacy check cares about (crash colours differently).
	signalKindBug   = "bug"
	signalKindCrash = "crash"

	// versionSourceLabel marks a version derived from a v/N.x label — the quote
	// for those is just "labelled v/N.x", which adds nothing over the source tag.
	versionSourceLabel = "label"
)

// requestLabels say outright that an issue is a request, whatever else it lacks.
var requestLabels = map[string]bool{"new-resource": true, "new-data-source": true, "enhancement": true}

// isRequestLabel reports whether a label marks the issue as a request.
func isRequestLabel(l string) bool { return requestLabels[strings.ToLower(l)] }

// LegacyOpts configures the legacy audit and its apply modes.
type LegacyOpts struct {
	Majors              []int // only bugs reported against these majors (empty = every legacy major)
	cli.FlagsApplyModes       // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// legacyFinding is one closeable legacy bug: the issue, its signals, and the
// rules-built close action (template, state reason, evidence all set).
type legacyFinding struct {
	issue   *db.Issue
	signals *db.Signals
	action  *db.Action

	// the rules could not tell what this issue is, so it is here on the
	// hypothesis that it is a bug: only the AI modes may act on it
	kindUnconfirmed bool
}

// Legacy finds OPEN bug and crash reports against legacy majors (v1..current-2)
// that the keep rules cleared for closing — no recent repro claim, no open PR,
// not highly engaged. The AI reads each issue AND its comments (where the gold
// is on old issues) and scores whether closing as stale is right; the apply
// modes close with the legacy-bug comment. Enhancements are a different
// problem and are not touched here.
func (f *Flags) Legacy() error {
	o := LegacyOpts{Majors: f.Cmd.LegacyMajors, FlagsApplyModes: f.Modes}
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

	col, err := f.collectLegacy(d, o.Majors)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d open bugs/crashes report a legacy major (v1–v%d):</>\n", col.legacyBugs, col.maxMajor)
	if col.unknownKind > 0 {
		cout.Printf("  <gray>%d of them are unlabelled and carried as probable bugs — the AI confirms the kind, so plain --apply skips them</>\n", col.unknownKind)
	}
	if col.divertedAsks > 0 {
		cout.Printf("  <gray>%d more are unlabelled but read as requests —</> <cyan>koi close exists</> <gray>territory, not this check's</>\n", col.divertedAsks)
	}
	var majors []string
	for m := 1; m <= col.maxMajor; m++ {
		if col.byMajor[m] > 0 {
			majors = append(majors, fmt.Sprintf("<lightMagenta>v%d</> <yellow>%d</>", m, col.byMajor[m]))
		}
	}
	cout.Printf("  <green>close candidates</> <yellow>%d</>  <gray>·</> %s\n", len(findings), strings.Join(majors, " <gray>·</> "))
	if len(col.protected) > 0 {
		parts := make([]string, 0, len(col.protected))
		for _, r := range text.SortedKeys(col.protected) {
			parts = append(parts, fmt.Sprintf("<gray>%s</> <yellow>%d</>", r, col.protected[r]))
		}
		cout.Printf("  <fg=208>protected</>        %s\n", strings.Join(parts, " <gray>·</> "))
	}
	if col.diverted > 0 {
		cout.Printf("  <gray>fixed-merged-pr  %d (koi close fixed closes these)</>\n", col.diverted)
	}
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyLegacy(d, findings, o, true)
	case o.Apply:
		return f.applyLegacy(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest-stale first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.legacyJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passLegacy, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b legacyFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without staleness scores</>\n")
	}

	for n := range findings {
		f.printLegacyCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close legacy --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// legacyCollection is everything the legacy sweep learns: the close-cleared
// findings plus the counts that explain what was NOT cleared.
type legacyCollection struct {
	findings     []legacyFinding
	byMajor      map[int]int
	protected    map[string]int // keep reason → count
	diverted     int            // fixed-merged-pr, koi close fixed territory
	legacyBugs   int            // every open bug/crash on a legacy major
	unknownKind  int            // of those, ones the rules could not identify
	divertedAsks int            // unlabelled but request-shaped: koi close exists territory
	maxMajor     int            // newest legacy major (current-2)
	open         int            // open-issue total
}

// collectLegacy builds the legacy findings: open bug/crash reports on legacy
// majors that the keep rules cleared for closing, optionally scoped to majors.
func (f *Flags) collectLegacy(d *db.DB, onlyMajors []int) (legacyCollection, error) {
	cfg := f.RuleConfig()
	col := legacyCollection{byMajor: map[int]int{}, protected: map[string]int{}, maxMajor: cfg.CurrentMajor - 2}

	issues, err := d.OpenIssues()
	if err != nil {
		return col, err
	}
	col.open = len(issues)
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return col, serr
		}
		if s == nil || s.VersionMajor < 1 || s.VersionMajor > col.maxMajor {
			continue
		}
		// the rules read kind from the labels and the issue template, and a
		// third of recent issues carry neither. Rather than leaving those
		// invisible, an old-version issue with NO kind is carried on the
		// hypothesis that it is a bug and the AI is asked to confirm that
		// before anything closes; a known question or docs issue still isn't
		// a legacy bug and is dropped here.
		rules, unconfirmed := s, false
		if s.Kind != signalKindBug && s.Kind != signalKindCrash {
			if s.Kind != "" {
				continue
			}
			// an unlabelled issue that READS as a request is not this check's
			// problem: koi close exists takes those on the same hypothesis, and a
			// feature request closed as a stale bug is the wrong close
			if existsAsk.MatchString(i.Title) || slices.ContainsFunc(i.Labels, isRequestLabel) {
				col.divertedAsks++
				continue
			}
			hypothesis := *s
			hypothesis.Kind = signalKindBug
			rules, unconfirmed = &hypothesis, true
		}
		col.legacyBugs++
		if unconfirmed {
			col.unknownKind++
		}

		a := issue.Propose(i, rules, cfg)
		switch {
		case a == nil:
			continue
		case a.Action == db.ActionKeep:
			col.protected[a.Reason]++
			continue
		case a.Reason == issue.ReasonFixedMergedPR:
			col.diverted++ // koi close fixed territory: a merged PR references it
			continue
		case a.Reason != issue.ReasonLegacyBug:
			continue
		}
		if len(onlyMajors) > 0 && !slices.Contains(onlyMajors, s.VersionMajor) {
			continue
		}
		col.findings = append(col.findings, legacyFinding{issue: i, signals: s, action: a, kindUnconfirmed: unconfirmed})
		col.byMajor[s.VersionMajor]++
	}
	return col, nil
}

// applyLegacy is both apply modes on the shared harness: plain --apply closes
// every rules-cleared candidate; --apply-with-ai[-auto] gates each close on
// the judge.
func (f *Flags) applyLegacy(d *db.DB, findings []legacyFinding, o LegacyOpts, withAI bool) error {
	if !withAI {
		// nothing here confirms the hypothesis that an unlabelled issue is a
		// bug, so those wait for an AI mode rather than being closed on a guess
		held := 0
		kept := findings[:0]
		for _, fdg := range findings {
			if fdg.kindUnconfirmed {
				held++
				continue
			}
			kept = append(kept, fdg)
		}
		findings = kept
		if held > 0 {
			cout.Printf("<gray>holding back %d unlabelled issue(s): --apply-with-ai confirms what they are</>\n", held)
		}
	}

	byNumber := map[int]*legacyFinding{}
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
			return f.closeOneLegacy(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "legacy bugs"
	p.GateLabel = "staleness"
	p.AllMode = "<gray>closing everything the rules cleared</>"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> legacy bugs as not planned in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close legacy bugs the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.legacyJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passLegacy, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneLegacy handles one candidate: card, the legacy-bug comment, and the
// close (or preview under dry-run, or the a/s ask when interactive).
func (f *Flags) closeOneLegacy(d *db.DB, repo gh.Repo, fdg *legacyFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printLegacyCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := issue.RenderCloseComment(fdg.issue, fdg.signals, fdg.action, f.CurrentMajor)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), fdg.action.Template, fdg.action.StateReason)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as a legacy bug?", fdg.issue.Number), comment, fdg.issue.URL)
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
	if err := repo.CloseIssue(fdg.issue.Number, fdg.action.StateReason); err != nil {
		cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", fdg.action.StateReason)
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, fdg.action.Reason)

	if v != nil {
		fdg.action.Confidence = v.Confidence
		fdg.action.Evidence[evidenceKeyAI] = v.Reason
	}
	if _, err := d.ProposeAction(fdg.action); err != nil {
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

// printLegacyCard is one candidate: the issue, its kind and version evidence,
// engagement, and the AI's staleness score when judged.
func (f *Flags) printLegacyCard(fdg *legacyFinding, pos, total int, v *issue.Verdict) {
	i, s := fdg.issue, fdg.signals
	kindTag, kind := cli.TagOrange, s.Kind
	if s.Kind == signalKindCrash {
		kindTag = cli.TagRed
	}
	if fdg.kindUnconfirmed {
		kindTag, kind = cli.TagYellow, "unlabelled, probably a bug"
	}
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, i.Number, text.StateTag(i.State), text.TruncateRunes(text.OneLine(i.Title), 90), f.IssueURL(i.Number))
	cout.Printf("      <%s>%s</> <gray>on</> <lightMagenta>v%s</> <gray>(%s)</> <gray>· opened %s · last activity %s · 💬 %d · 👍 %d</>\n",
		kindTag, kind, text.OrDefault(s.VersionFull, fmt.Sprintf("%d.x", s.VersionMajor)), s.VersionSource,
		i.CreatedAt.Format("2006-01-02"), s.LastActivity.Format("2006-01-02"), i.CommentCount, i.ThumbsUp)
	if s.VersionQuote != "" {
		cout.Printf("      <gray>version evidence:</> %s\n", text.TruncateRunes(text.OneLine(s.VersionQuote), 100))
	}
	cli.PrintVerdict(v)
}

// legacyJudgeItems renders one judge block per candidate: the issue's body and
// a digest of its comments — the comments are where "still happens on v5"
// hides, so the AI reads them before blessing a close.
func (f *Flags) legacyJudgeItems(d *db.DB, findings []legacyFinding) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptLegacy)
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
		fmt.Fprintf(&b, "reported version: v%s (%s): %s\n",
			text.OrDefault(fdg.signals.VersionFull, fmt.Sprintf("%d.x", fdg.signals.VersionMajor)),
			fdg.signals.VersionSource, text.OneLine(fdg.signals.VersionQuote))
		kind := fdg.signals.Kind
		if fdg.kindUnconfirmed {
			kind = "UNKNOWN — nothing labels this issue, so judge whether it is a bug or crash report at all"
		}
		fmt.Fprintf(&b, "kind: %s · opened %s · last activity %s\n",
			kind, fdg.issue.CreatedAt.Format("2006-01-02"), fdg.signals.LastActivity.Format("2006-01-02"))
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))

		picked := issue.DigestComments(comments, 8)
		if len(picked) > 0 {
			fmt.Fprintf(&b, "COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}
