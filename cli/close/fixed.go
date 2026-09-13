// The fixed check: open issues a shipped code change appears to fix — a
// merged same-repo PR references the issue, or (the changelog side, once its
// own check) a BUG FIXES bullet on the issue's resource postdates the report
// with no PR ever citing the issue.

package close

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

const (
	passFixed   = "fixed"
	promptFixed = "issue-fixed"

	// evidence classes strongest first: PR references (fixed-by is a
	// closing-keyword reference, mentioned-by a bare mention, matching the
	// milestone ux), then uncited changelog bullets (matched shares the
	// report's own property or symptom, resource-only merely touched the
	// resource).
	classFixedBy        = "fixed-by"
	classMentionedBy    = "mentioned-by"
	classClMatched      = "matched"
	classClResourceOnly = "resource-only"

	templateFixedShipped   = "fixed-shipped"
	templateChangelogClose = "changelog-close"
	reasonChangelogFixed   = "changelog-fixed"

	// prLabelMerged is shared between the state labels and the fixed subcommand.
	prLabelMerged = "merged"

	// evidence key shared across the closing commands.
	evidenceKeyVersion = "version"

	// how many ranked bullets a card and judge block carry — chatty resources
	// (network, compute) can accrue dozens of post-report fixes
	changelogMaxBullets = 6

	// the substance score at which bullet evidence is matched rather than
	// resource-only: one shared property token, or two shared symptom words
	changelogMatchedScore = 2
)

var fixedClassRank = map[string]int{classFixedBy: 3, classMentionedBy: 2, classClMatched: 1, classClResourceOnly: 0}

// FixedOpts configures the fixed audit and its apply modes.
type FixedOpts struct {
	Link                string // fixed-by | mentioned-by | matched | resource-only ("" = all)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// changelogBullet is one post-report BUG FIXES bullet on one of the issue's
// resources, scored by how much of the issue's substance it shares.
type changelogBullet struct {
	entry db.ChangelogEntry
	score int
}

// fixedFinding is one open issue with its fix evidence: the merged same-repo
// PRs referencing it, and the uncited post-report changelog bullets on its
// resources. Either list may be empty, never both.
type fixedFinding struct {
	issue      *db.Issue
	prs        []db.Crossref
	bullets    []changelogBullet // ranked best-substance first, capped
	reported   string            // full version the issue reported against ("" = major only or unknown)
	major      int               // reported major (0 = unknown)
	class      string            // strongest evidence class present
	best       db.Crossref       // the PR the close comment cites (when prs exist)
	version    string            // earliest release shipping best ("" when unreleased)
	reopenedBy int               // PR whose merge closed this issue before it was reopened (0 = never)
}

// reChangelogSnake pulls property-shaped tokens from a bullet or an issue:
// snake_case identifiers, the strongest substance signal there is. Resource
// names themselves are excluded by the caller — matching on those is the
// sweep's job, not evidence of substance.
var reChangelogSnake = regexp.MustCompile(`[a-z0-9]+(?:_[a-z0-9]+)+`)

// changelogSymptoms are the words a fix description and a bug report share
// when they describe the same failure shape. Deliberately short — generic
// words (error, update, fix) appear everywhere and prove nothing.
var changelogSymptoms = []string{
	"crash", "panic", "import", "recreat", "replace", "destroy", "delete",
	"timeout", "diff", "casing", "case sensitiv", "parse", "force new",
	"forcenew", "perpetual", "idempoten", "drift", "nil",
}

// changelogScore rates one bullet against the issue's words: +3 per shared
// property token, +1 per shared symptom word.
func changelogScore(bullet string, issueWords map[string]bool, issueLow string, resources map[string]bool) int {
	low := strings.ToLower(bullet)
	score := 0
	seen := map[string]bool{}
	for _, tok := range reChangelogSnake.FindAllString(low, -1) {
		if seen[tok] || resources[tok] || strings.HasPrefix(tok, "azurerm_") {
			continue
		}
		seen[tok] = true
		if issueWords[tok] {
			score += 3
		}
	}
	for _, s := range changelogSymptoms {
		if strings.Contains(low, s) && strings.Contains(issueLow, s) {
			score++
		}
	}
	return score
}

// fixedCollection is everything collectFixed learns in one scan.
type fixedCollection struct {
	findings   []fixedFinding
	counts     map[string]int
	prVersions map[int][]string
	open       int            // open issues in the db
	bugs       int            // open bug/crash reports swept for bullets
	predated   int            // bullet-only candidates whose every fix predated the reported version
	protected  map[string]int // bullet-only candidates kept out by the guards
}

// Fixed lists every OPEN issue a shipped fix appears to cover: a merged
// same-repo PR references it (fixed-by, mentioned-by), or a BUG FIXES bullet
// on its resource postdates the report with no PR citing the issue (matched,
// resource-only — the fixes nobody linked). The AI judges every piece of fix
// evidence together on full text; the apply modes close the confirmed ones as
// completed citing the PR and shipped release, or the bullet and its release.
func (f *Flags) Fixed(link string) error {
	o := FixedOpts{Link: link, FlagsApplyModes: f.Modes}
	if err := f.RequireAIEarly(); err != nil {
		return err
	}
	// stay fresh by default: the incremental fetch is cheap and stale crossref
	// or issue state here means judging (or closing!) on old information
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

	col, err := f.collectFixed(d, o.Link)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d of %d open issues have a shipped fix that may cover them:</>\n", len(findings), col.open)
	for _, c := range []struct{ class, tag, note string }{
		{classFixedBy, cli.TagGreen, "a merged PR says it closes this issue"},
		{classMentionedBy, cli.TagOrange, "a merged PR mentions this issue"},
		{classClMatched, cli.TagYellow, "an uncited fix description shares the bug's own property or symptom"},
		{classClResourceOnly, cli.TagGray, "uncited later fixes touched the resource, nothing lines up textually"},
	} {
		if n := col.counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-13s</> <yellow>%d</>  <gray>%s</>\n", c.tag, c.class, n, c.note)
		}
	}
	cout.Printf("  <gray>bullet sweep: %d bug reports scanned · %d with every fix predating the reported version · %s</>\n",
		col.bugs, col.predated, keepSummary(col.protected))
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyFixed(d, findings, col.prVersions, o, true)
	case o.Apply:
		// plain --apply closes what's listed with no AI involved
		return f.applyFixed(d, findings, col.prVersions, o, false)
	}

	// report: score everything (pipelined, cached) and list best matches first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.fixedJudgeItems(d, findings, col.prVersions)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passFixed, promptText, items, nil, nil); err != nil {
			return err
		}
		cli.SortByVerdict(findings, func(x *fixedFinding) int { return x.issue.Number }, verdicts)
	} else {
		cout.Printf("<gray>--ai=false: listing without match scores</>\n")
	}

	for n := range findings {
		fdg := &findings[n]
		f.printFixedCard(fdg, n+1, len(findings), col.prVersions, verdicts[fdg.issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close fixed --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// collectFixed builds the findings from both evidence sources: every open
// issue a merged same-repo PR references, and — for bug/crash reports — the
// uncited BUG FIXES bullets on their resources that postdate both the report
// (the shared issue/PR number space orders them) and the version it reported
// against. An issue with both gets one finding carrying everything.
func (f *Flags) collectFixed(d *db.DB, link string) (*fixedCollection, error) {
	col := &fixedCollection{counts: map[string]int{}, protected: map[string]int{}}
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	col.open = len(issues)
	col.prVersions, err = d.ChangelogVersionsByPR()
	if err != nil {
		return nil, err
	}
	// the milestone scan knows which PR's merge CLOSED an issue — an open issue
	// with one was reopened, which colours whether the fix actually stuck
	msFixes, err := d.MSFixesByIssue()
	if err != nil {
		return nil, err
	}

	cout.Printf("scanning open bug reports for post-report changelog fixes on their resources...\n")
	for _, i := range issues {
		refs, cerr := d.CrossrefsFor(i.Number)
		if cerr != nil {
			return nil, cerr
		}
		var prs []db.Crossref
		cited := map[int]bool{}
		for _, r := range refs {
			if r.IsPR {
				cited[r.RefNumber] = true
			}
			if r.IsPR && r.Merged && strings.EqualFold(r.RefRepo, f.GH.Repo) {
				prs = append(prs, r)
			}
		}

		bullets, reported, major, berr := f.fixedBullets(d, i, cited, col)
		if berr != nil {
			return nil, berr
		}
		if len(prs) == 0 && len(bullets) == 0 {
			continue
		}

		fdg := fixedFinding{issue: i, prs: prs, bullets: bullets, reported: reported, major: major}
		switch {
		case len(prs) > 0:
			fdg.class = classMentionedBy
			for _, pr := range prs {
				if pr.WillClose {
					fdg.class = classFixedBy
				}
			}
		case bullets[0].score >= changelogMatchedScore:
			fdg.class = classClMatched
		default:
			fdg.class = classClResourceOnly
		}

		if len(prs) > 0 {
			// the comment cites the strongest, earliest-shipped reference:
			// closing beats mentioning, then a shipped reference beats an
			// unshipped one, then the earliest release wins
			fdg.best = prs[0]
			if vs := col.prVersions[fdg.best.RefNumber]; len(vs) > 0 {
				fdg.version = vs[0]
			}
			for _, pr := range prs[1:] {
				vs := col.prVersions[pr.RefNumber]
				stronger := pr.WillClose && !fdg.best.WillClose
				earlier := pr.WillClose == fdg.best.WillClose && len(vs) > 0 &&
					(fdg.version == "" || issue.VersionLess(vs[0], fdg.version))
				if !stronger && !earlier {
					continue
				}
				fdg.best = pr
				fdg.version = ""
				if len(vs) > 0 {
					fdg.version = vs[0]
				}
			}
		}
		for _, fx := range msFixes[i.Number] {
			if fx.Link == db.LinkClosedBy {
				fdg.reopenedBy = fx.PRNumber
			}
		}
		if link != "" && fdg.class != link {
			continue
		}
		col.findings = append(col.findings, fdg)
		col.counts[fdg.class]++
	}

	slices.SortStableFunc(col.findings, func(a, b fixedFinding) int {
		if d := fixedClassRank[b.class] - fixedClassRank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return col, nil
}

// fixedBullets sweeps one issue for uncited post-report BUG FIXES bullets on
// its resources: bug and crash reports only, fix PR newer than the issue,
// release newer than the reported version, PRs the issue cross-references
// excluded (they are already PR evidence). Guarded issues (open linked PR,
// high engagement) keep their PR evidence but surface no bullet-only finding.
func (f *Flags) fixedBullets(d *db.DB, i *db.Issue, cited map[int]bool, col *fixedCollection) (bullets []changelogBullet, reported string, major int, err error) {
	s, err := d.GetSignals(i.Number)
	if err != nil {
		return nil, "", 0, err
	}
	if s == nil || (s.Kind != "bug" && s.Kind != "crash") || len(s.Resources) == 0 {
		return nil, "", 0, nil
	}
	col.bugs++

	// the reported version: signals keeps only its best-precedence pick and a
	// v/N.x label wins that precedence, so re-parse the body for the full
	// version the way the version labeller does
	reported, major = s.VersionFull, s.VersionMajor
	if reported == "" {
		if vm := issue.ExtractProviderVersion(i.Body); vm != nil && vm.Full != "" {
			reported, major = vm.Full, vm.Major
		}
	}

	issueLow := strings.ToLower(i.Title + "\n" + issue.Prose(issue.CleanBody(i.Body)))
	issueWords := map[string]bool{}
	for _, tok := range reChangelogSnake.FindAllString(issueLow, -1) {
		issueWords[tok] = true
	}
	resources := map[string]bool{}
	for _, r := range s.Resources {
		resources[strings.TrimPrefix(r, "data.")] = true
	}

	hadCandidate := false
	for r := range resources {
		entries, eerr := d.ChangelogFor(r)
		if eerr != nil {
			return nil, "", 0, eerr
		}
		for _, e := range entries {
			if !strings.Contains(e.Section, "BUG") || e.PRNumber == 0 || e.PRNumber <= i.Number || cited[e.PRNumber] {
				continue
			}
			hadCandidate = true
			// a fix that shipped at or before the reported version cannot be
			// the answer — they hit the bug with that fix already in
			if reported != "" && !issue.VersionLess(reported, e.Version) {
				continue
			}
			if reported == "" && major > 0 && e.Major < major {
				continue
			}
			bullets = append(bullets, changelogBullet{entry: e, score: changelogScore(e.Text, issueWords, issueLow, resources)})
		}
	}
	if len(bullets) == 0 {
		if hadCandidate {
			col.predated++
		}
		return nil, reported, major, nil
	}

	switch {
	case s.OpenLinkedPRs > 0:
		col.protected["open-pr"]++
		return nil, reported, major, nil
	case i.ThumbsUp >= f.KeepReactions:
		col.protected["high-engagement"]++
		return nil, reported, major, nil
	}

	// best substance first, newest release breaking ties; cap the tail
	slices.SortStableFunc(bullets, func(a, b changelogBullet) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if issue.VersionLess(a.entry.Version, b.entry.Version) {
			return 1
		}
		return -1
	})
	if len(bullets) > changelogMaxBullets {
		bullets = bullets[:changelogMaxBullets]
	}
	return bullets, reported, major, nil
}

// applyFixed is both apply modes on the shared harness: plain --apply closes
// everything listed; --apply-with-ai[-auto] gates each close on the judge.
func (f *Flags) applyFixed(d *db.DB, findings []fixedFinding, prVersions map[int][]string, o FixedOpts, withAI bool) error {
	byNumber := map[int]*fixedFinding{}
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
			return f.closeOneFixed(d, repo, byNumber[n], v, pos, total, prVersions, throttle, interactive)
		})
	p.Noun = "candidates as fixed"
	p.GateLabel = "match"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as completed in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.fixedJudgeItems(d, findings, prVersions)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passFixed, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneFixed handles one candidate: card, comment, and the close itself (or
// a preview under dry-run, or the a/s ask when interactive). The comment cites
// the best PR when there is one, the best bullet otherwise.
func (f *Flags) closeOneFixed(d *db.DB, repo gh.Repo, fdg *fixedFinding, v *issue.Verdict, pos, total int, prVersions map[int][]string, throttle func(), ask bool) (int, error) {
	f.printFixedCard(fdg, pos, total, prVersions, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	tmplName, reason := templateFixedShipped, issue.ReasonFixedMergedPR
	if len(fdg.prs) == 0 {
		tmplName, reason = templateChangelogClose, reasonChangelogFixed
	}
	comment, err := f.renderFixedComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), tmplName, issue.StateCompleted)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as fixed?", fdg.issue.Number), comment, fdg.issue.URL)
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
	if err := repo.CloseIssue(fdg.issue.Number, issue.StateCompleted); err != nil {
		cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", issue.StateCompleted)
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reason)
	return issue.ApplySet, f.recordFixedClose(d, fdg, v, tmplName, reason)
}

// printFixedCard is one finding: the open issue, its merged PR references and
// uncited fix bullets, the reopen callout when the scan saw one, and the AI's
// score when judged.
func (f *Flags) printFixedCard(fdg *fixedFinding, pos, total int, prVersions map[int][]string, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	for i := range fdg.prs {
		cout.Printf("      %s\n", fixedPRLine(&fdg.prs[i], prVersions))
	}
	if len(fdg.bullets) > 0 {
		rep := "version unknown"
		switch {
		case fdg.reported != "":
			rep = "v" + fdg.reported
		case fdg.major > 0:
			rep = fmt.Sprintf("v%d.x", fdg.major)
		}
		cout.Printf("      <gray>reported against</> <lightMagenta>%s</><gray>, uncited fixes shipped since:</>\n", rep)
		for _, bl := range fdg.bullets {
			tag := "gray"
			if bl.score >= changelogMatchedScore {
				tag = cli.TagGreen
			}
			cout.Printf("        <%s>v%-8s</> %s\n", tag, bl.entry.Version, text.TruncateRunes(text.OneLine(bl.entry.Text), 120))
		}
	}
	if fdg.reopenedBy != 0 {
		cout.Printf("      <red>closed by PR #%d and then reopened</>\n", fdg.reopenedBy)
	}
	cli.PrintVerdict(v)
}

// recordFixedClose writes the applied action row for one close.
func (f *Flags) recordFixedClose(d *db.DB, fdg *fixedFinding, v *issue.Verdict, tmplName, reason string) error {
	evidence := map[string]string{}
	if len(fdg.prs) > 0 {
		evidence["pr"] = fmt.Sprintf("#%d", fdg.best.RefNumber)
		evidence[evidenceKeyVersion] = fdg.version
	} else {
		best := &fdg.bullets[0]
		evidence["bullet"] = text.TruncateRunes(text.OneLine(best.entry.Text), 160)
		evidence[evidenceKeyVersion] = best.entry.Version
		evidence["fix-pr"] = strconv.Itoa(best.entry.PRNumber)
	}
	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reason,
		StateReason: issue.StateCompleted, Template: tmplName,
		Evidence:       evidence,
		Source:         passFixed,
		IssueUpdatedAt: fdg.issue.UpdatedAt,
	}
	if v != nil {
		a.Confidence = v.Confidence
		a.Evidence[evidenceKeyAI] = v.Reason
	}
	if _, err := d.ProposeAction(a); err != nil {
		return err
	}
	row, err := d.GetAction(fdg.issue.Number)
	if err != nil || row == nil {
		return err
	}
	if row.Status == db.StatusProposed {
		if err := d.DecideAction(row.ID, db.StatusApproved, f.Decider()); err != nil {
			return err
		}
	}
	return d.MarkApplied(row.ID, db.StatusApplied, "")
}

// renderFixedComment renders the close comment: the fix PR and shipped
// version when a PR references the issue, the best bullet and its release
// otherwise (the bullet text already carries the fix PR link).
func (f *Flags) renderFixedComment(fdg *fixedFinding) (string, error) {
	name := templateFixedShipped
	if len(fdg.prs) == 0 {
		name = templateChangelogClose
	}
	tt, err := assets.CommentTemplate(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", name, err)
	}
	var data any
	if len(fdg.prs) > 0 {
		data = struct {
			PR           int
			PRTitle      string
			Version      string
			CurrentMajor int
		}{fdg.best.RefNumber, text.OneLine(fdg.best.Title), fdg.version, f.CurrentMajor}
	} else {
		best := &fdg.bullets[0]
		data = struct {
			Version      string
			Bullet       string
			Repo         string
			CurrentMajor int
		}{best.entry.Version, strings.TrimSpace(best.entry.Text), f.GH.Repo, f.CurrentMajor}
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// fixedPRLine renders one merged reference: class-coloured link strength, the
// shipping release when the changelog knows it, and the PR title.
func fixedPRLine(pr *db.Crossref, prVersions map[int][]string) string {
	var b strings.Builder
	if pr.WillClose {
		fmt.Fprintf(&b, "<%s>fixed by</> PR <lightCyan>#%d</>", cli.TagGreen, pr.RefNumber)
	} else {
		fmt.Fprintf(&b, "<%s>mentioned by</> PR <lightCyan>#%d</>", cli.TagOrange, pr.RefNumber)
	}
	if vs := prVersions[pr.RefNumber]; len(vs) > 0 {
		fmt.Fprintf(&b, " <gray>— shipped in</> <lightMagenta>v%s</>", vs[0])
	} else {
		fmt.Fprintf(&b, " <gray>— %s, not yet in a release</>", prLabelMerged)
	}
	fmt.Fprintf(&b, " <gray>·</> %s", text.TruncateRunes(text.OneLine(pr.Title), 70))
	return b.String()
}

// fixedJudgeItems fetches the PR texts and renders one judge block per
// finding: the issue's title, body, and thread digest, every merged reference
// with its shipping release and body, every uncited bullet with its release,
// and the reopen note when the scan saw one.
func (f *Flags) fixedJudgeItems(d *db.DB, findings []fixedFinding, prVersions map[int][]string) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptFixed)
	if err != nil {
		return "", nil, err
	}

	prNumbers := map[int]bool{}
	for i := range findings {
		for _, pr := range findings[i].prs {
			prNumbers[pr.RefNumber] = true
		}
	}
	if err := f.FetchTexts(d, text.SortedKeys(prNumbers)); err != nil {
		return "", nil, err
	}
	texts, err := d.Texts()
	if err != nil {
		return "", nil, err
	}

	items := make([]issue.JudgeItem, 0, len(findings))
	for i := range findings {
		fdg := &findings[i]
		var b strings.Builder
		fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.issue.Number, text.OneLine(fdg.issue.Title))
		fmt.Fprintf(&b, "opened %s, last activity %s\n", fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		switch {
		case fdg.reported != "":
			fmt.Fprintf(&b, "REPORTED AGAINST: azurerm v%s\n", fdg.reported)
		case fdg.major > 0:
			fmt.Fprintf(&b, "REPORTED AGAINST: azurerm v%d.x (exact version unknown)\n", fdg.major)
		}
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		comments, cerr := d.CommentsFor(fdg.issue.Number)
		if cerr != nil {
			return "", nil, cerr
		}
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "ISSUE COMMENTS (%d of %d — watch for still-broken claims after a fix shipped):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		if len(fdg.prs) > 0 {
			b.WriteString("REFERENCED PRS:\n")
			for _, pr := range fdg.prs {
				state := prLabelMerged
				if vs := prVersions[pr.RefNumber]; len(vs) > 0 {
					state = "merged, shipped in v" + vs[0]
				}
				link := ""
				if pr.WillClose {
					link = ", closing-keyword link"
				}
				fmt.Fprintf(&b, "- PR #%d (%s%s): %s\n", pr.RefNumber, state, link, text.OneLine(pr.Title))
				if t, ok := texts[pr.RefNumber]; ok && t.Body != "" {
					fmt.Fprintf(&b, "  PR BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(t.Body), cli.PRBodyRunes))
				}
			}
		}
		if len(fdg.bullets) > 0 {
			b.WriteString("UNCITED BUG-FIX BULLETS ON THIS ISSUE'S RESOURCES (post-report, no PR cites this issue; best substance match first):\n")
			for _, bl := range fdg.bullets {
				fmt.Fprintf(&b, "- [v%s] %s\n", bl.entry.Version, text.OneLine(bl.entry.Text))
			}
		}
		if fdg.reopenedBy != 0 {
			fmt.Fprintf(&b, "NOTE: this issue was closed by PR #%d and then REOPENED — the fix may have been incomplete or regressed.\n", fdg.reopenedBy)
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}
