package milestone

import (
	"encoding/csv"
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

// MilestoneOpts configures the milestone audit.
type MilestoneOpts struct {
	cli.FlagsMilestone         // --skip-scan / --rescan / --csv / --bucket
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
	Link                string // determine milestones from this evidence class only ("" = strongest available)
}

// Milestone audits every issue in the repo — open AND closed — against the
// milestone it should carry. The expected milestone is derived from merged PRs
// that cross-reference the issue, mapped to the release that shipped them via
// the changelog (which links every bullet to its PR number).
func (f *Flags) Milestone(link string) error {
	o := MilestoneOpts{FlagsMilestone: f.Cmd.MS, FlagsApplyModes: f.Modes, Link: link}
	// only the --apply-with-ai modes judge here (the listing and plain --apply
	// never do), so fail a missing AI config before the scan, not after it
	if o.ApplyWithAI || o.ApplyWithAIAuto {
		if err := f.RequireAI(); err != nil {
			return err
		}
	}
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	if !o.SkipScan && !f.NoAutoFetch {
		if err := f.MilestoneScan(d, o.Rescan); err != nil {
			return err
		}
	}

	return f.milestoneAudit(d, o)
}

// ---- audit ----

// audit buckets.
const (
	msMissing       = "missing"       // closed, no milestone, expected determinable
	msMismatch      = "mismatch"      // has a milestone that differs from the determined one
	msOpenReleased  = "open-released" // open issue sitting on an already-released milestone
	msUndetermined  = "undetermined"  // closed as completed, no milestone, no changelog-mapped fix
	msNoSuchRelease = "no-milestone"  // determinable version but no matching milestone exists
	msOK            = "ok"
)

type msFinding struct {
	issue       db.MSIssue
	bucket      string
	expected    string // milestone title, e.g. "v4.81.0"
	noMilestone bool   // expected is determinable but no such milestone exists — create it to fix
	fixPRs      []int
	via         []db.MSFix // the fix PRs behind expected, at the winning link strength
	cited       bool       // expected came from the changelog citing the issue number itself
	reason      string     // the above as a sentence, e.g. "closed by PR #1564 — in the v1.10.0 changelog"
}

// msCollection is everything one audit pass learns.
type msCollection struct {
	total       int
	findings    []msFinding // everything actionable or listable (not ok/undetermined)
	counts      map[string]int
	classCounts map[string]map[string]int
	milestones  map[string]db.Milestone
}

// collectMilestone runs the audit over every scanned issue and buckets the
// findings; link restricts which evidence class may determine a milestone.
func (f *Flags) collectMilestone(d *db.DB, link string) (*msCollection, error) {
	col := &msCollection{counts: map[string]int{}, classCounts: map[string]map[string]int{}}
	issues, err := d.MSIssues()
	if err != nil {
		return nil, err
	}
	col.total = len(issues)
	if col.total == 0 {
		return col, nil
	}
	fixes, err := d.MSFixesByIssue()
	if err != nil {
		return nil, err
	}
	prVersions, err := d.ChangelogVersionsByPR()
	if err != nil {
		return nil, err
	}
	if col.milestones, err = d.Milestones(); err != nil {
		return nil, err
	}
	if len(prVersions) == 0 {
		cout.Errorf("<yellow>warning:</> changelog table is empty — run <cyan>koi fetch</> first for fix→release mapping\n")
	}

	for _, i := range issues {
		fdg := auditIssue(i, fixes[i.Number], prVersions, col.milestones, link)
		col.counts[fdg.bucket]++
		if class := fdg.linkClass(); class != "" {
			if col.classCounts[fdg.bucket] == nil {
				col.classCounts[fdg.bucket] = map[string]int{}
			}
			col.classCounts[fdg.bucket][class]++
		}
		if fdg.bucket != msOK && fdg.bucket != msUndetermined {
			col.findings = append(col.findings, fdg)
		}
	}
	return col, nil
}

func (f *Flags) milestoneAudit(d *db.DB, o MilestoneOpts) error {
	col, err := f.collectMilestone(d, o.Link)
	if err != nil {
		return err
	}
	if col.total == 0 {
		cout.Printf("no scanned issues — run <cyan>koi milestone</> without --skip-scan first\n")
		return nil
	}
	findings, counts, classCounts, milestones := col.findings, col.counts, col.classCounts, col.milestones

	cout.Printf("\n<bold>milestone audit over %d issues:</>\n", col.total)
	for _, k := range text.SortedKeys(counts) {
		cout.Printf("  %-16s <yellow>%d</>\n", k, counts[k])
		// missing and mismatch get the class split: they're the buckets --apply
		// acts on, so the split is the wave plan — the other buckets are
		// report-only noise
		if cc := classCounts[k]; (k == msMissing || k == msMismatch) && len(cc) > 0 {
			parts := make([]string, 0, len(cc))
			for _, class := range []string{db.LinkClosedBy, db.LinkLinked, cli.LinkCited, db.LinkMention} {
				if n := cc[class]; n > 0 {
					parts = append(parts, fmt.Sprintf("<%s>%s</> <yellow>%d</>", cli.ClassTag(class), class, n))
				}
			}
			cout.Printf("      %s\n", strings.Join(parts, " <gray>·</> "))
		}
	}

	if o.Bucket != "" {
		listed := 0
		for _, fdg := range findings {
			if fdg.bucket != o.Bucket {
				continue
			}
			listed++
			f.printFinding(&fdg)
		}
		if listed == 0 {
			cout.Printf("no findings in bucket %q (listable: missing, mismatch, open-released, no-milestone)\n", o.Bucket)
		}
	} else {
		// examples per bucket so the console output stays scannable; --csv has it all
		shown := map[string]int{}
		for _, fdg := range findings {
			if shown[fdg.bucket] >= 10 {
				continue
			}
			shown[fdg.bucket]++
			f.printFinding(&fdg)
		}
	}

	if o.CSV != "" {
		if err := f.writeMilestoneCSV(o.CSV, findings); err != nil {
			return err
		}
		cout.Printf("wrote <cyan>%s</> (%d findings)\n", o.CSV, len(findings))
	} else if o.Bucket == "" && len(findings) > 10 {
		cout.Printf("<gray>(showing up to 10 per bucket — use</> <cyan>--csv audit.csv</> <gray>for the full list,</> <cyan>--bucket <name></> <gray>for one bucket)</>\n")
	}

	wants := map[string]bool{}
	for i := range findings {
		if fdg := &findings[i]; fdg.noMilestone {
			wants[fdg.expected] = true
		}
	}
	f.printMissingMilestones(text.SortedKeys(wants))

	if o.Apply || o.ApplyWithAI || o.ApplyWithAIAuto {
		return f.applyMilestones(d, findings, milestones, o)
	}
	if o.Bucket == "" && counts[msMissing]+counts[msMismatch] > 0 {
		cout.Printf("next: <cyan>koi milestone --skip-scan --apply --dry-run</> to preview, then drop <cyan>--dry-run</> to %s\n", applyHint(counts[msMissing], counts[msMismatch], "milestones"))
	}
	return nil
}

// auditIssue determines the expected milestone for one issue and buckets it.
// linkFilter restricts which evidence class may *determine* a milestone
// (closed-by | linked | mention | cited; "" = strongest class that yields one);
// ok/mismatch always compare against every candidate so a weaker link can still
// vindicate an existing milestone.
func auditIssue(i db.MSIssue, fixes []db.MSFix, prVersions map[int][]string, milestones map[string]db.Milestone, linkFilter string) msFinding {
	fdg := msFinding{issue: i, bucket: msOK}
	for _, fx := range fixes {
		fdg.fixPRs = append(fdg.fixPRs, fx.PRNumber)
	}

	// all candidate releases, for matching an existing milestone: every changelog
	// release shipping any fix PR, plus releases whose changelog cites the issue
	// number itself
	candidates := map[string]bool{}
	for _, fx := range fixes {
		for _, v := range prVersions[fx.PRNumber] {
			candidates[v] = true
		}
	}
	for _, v := range prVersions[i.Number] {
		candidates[v] = true
	}

	// expected milestone: work down the evidence classes — the PR that closed the
	// issue, then closing-keyword links, then the changelog citing the issue,
	// then bare mentions — and take the earliest release the winning class maps
	// to (the first shipped fix). A linkFilter pins the class instead.
	classes := []string{db.LinkClosedBy, db.LinkLinked, cli.LinkCited, db.LinkMention}
	if linkFilter != "" {
		classes = []string{linkFilter}
	}
	expected := ""
	for _, class := range classes {
		var via []db.MSFix
		if class == cli.LinkCited {
			for _, v := range prVersions[i.Number] {
				if expected == "" || issue.VersionLess(v, expected) {
					expected = v
				}
			}
			fdg.cited = expected != ""
		} else {
			for _, fx := range fixes {
				if fx.Link != class {
					continue
				}
				for _, v := range prVersions[fx.PRNumber] {
					if expected == "" || issue.VersionLess(v, expected) {
						expected = v
					}
				}
			}
			for _, fx := range fixes {
				if fx.Link == class && slices.Contains(prVersions[fx.PRNumber], expected) {
					via = append(via, fx)
				}
			}
		}
		if expected != "" {
			fdg.via = via
			break
		}
	}
	if expected != "" {
		fdg.expected = "v" + expected
		_, ok := milestones[fdg.expected]
		fdg.noMilestone = !ok
	}

	current := i.Milestone

	switch {
	case i.State == db.IssueClosed && current == "" && expected != "":
		if _, ok := milestones[fdg.expected]; ok {
			fdg.bucket = msMissing
		} else {
			fdg.bucket = msNoSuchRelease
		}
	case current != "" && expected != "" && !candidates[normalizeMilestone(current)]:
		fdg.bucket = msMismatch
	case i.State == db.IssueOpen && current != "" && milestones[current].State == db.IssueClosed:
		fdg.bucket = msOpenReleased
	case i.State == db.IssueClosed && i.StateReason == "COMPLETED" && current == "" && expected == "":
		fdg.bucket = msUndetermined
	}

	fdg.reason = msReason(&fdg)
	return fdg
}

// syncFixPRMilestones brings the fix PRs behind a just-set milestone into line:
// the milestone was determined FROM those PRs' changelog entries, so the PR
// carries the same bookkeeping gap the issue had — filled when missing,
// corrected when different (the changelog wins there too).
func (f *Flags) syncFixPRMilestones(repo gh.Repo, fdg *msFinding, m *db.Milestone, throttle func()) {
	for i := range fdg.via {
		pr := fdg.via[i].PRNumber
		live, err := repo.GetIssue(pr) // the issues endpoint serves PRs too
		if err != nil {
			cout.Errorf("      <red>checking fix PR #%d: %v</>\n", pr, err)
			continue
		}
		if live.Milestone != nil && live.Milestone.Title == fdg.expected {
			continue
		}
		throttle()
		if err := repo.SetMilestone(pr, m.Number); err != nil {
			cout.Errorf("      <red>setting milestone on fix PR #%d: %v</>\n", pr, err)
			continue
		}
		if live.Milestone == nil {
			cout.Printf("      <fg=208>fix PR #%d was missing it too — set milestone → %s</>\n", pr, fdg.expected)
		} else {
			cout.Printf("      <fg=208>fix PR #%d carried %s — set milestone → %s</>\n", pr, live.Milestone.Title, fdg.expected)
		}
		cout.QuietOnlyf("%d@pr-milestone@%s\n", pr, fdg.expected)
	}
}

// linkClass is the evidence class that determined this finding's expected
// milestone ("" when nothing did).
func (fdg *msFinding) linkClass() string {
	switch {
	case len(fdg.via) > 0:
		return fdg.via[0].Link
	case fdg.cited:
		return cli.LinkCited
	default:
		return ""
	}
}

// printFinding is one audit finding on one line, url in dark gray at the end so
// checking on github is a click away.
func (f *Flags) printFinding(fdg *msFinding) {
	current := fdg.issue.Milestone
	if current == "" {
		current = "(none)"
	}
	note := ""
	if fdg.noMilestone {
		note = fmt.Sprintf(" <red>no %s milestone — create it to fix</>", fdg.expected)
	}
	cout.Printf("  <gray>%-14s</> <cyan>#%d</> %s → <lightMagenta>%s</> <gray>(</>%s<gray>)</>%s %s <darkGray>%s</>\n",
		fdg.bucket, fdg.issue.Number, current, cli.OrDash(fdg.expected), msReasonColoured(fdg),
		note, text.TruncateRunes(fdg.issue.Title, 60), f.IssueURL(fdg.issue.Number))
}

// reChangelogRef strips the trailing PR link a changelog bullet carries —
// "([#1564](https://…))" or "[GH-1564]" — pure noise next to the PR number we
// already print.
var reChangelogRef = regexp.MustCompile(`\s*\(?\[(?:GH-|#)\d+\](?:\([^)]*\))?\)?`)

func changelogBullet(bullet string) string {
	return strings.TrimSpace(reChangelogRef.ReplaceAllString(bullet, ""))
}

// cli.LinkCited is the pseudo evidence class for "the changelog cites the issue
// number itself" — no PR link involved.

// linkPhrase describes one fix PR at its link strength, e.g. "closed by PR #123".
func linkPhrase(fx *db.MSFix) string {
	switch fx.Link {
	case db.LinkClosedBy:
		return fmt.Sprintf("closed by PR #%d", fx.PRNumber)
	case db.LinkLinked:
		return fmt.Sprintf("linked fix PR #%d", fx.PRNumber)
	default:
		return fmt.Sprintf("PR #%d (mentions this issue)", fx.PRNumber)
	}
}

// linkPhraseColoured is linkPhrase with the PR number highlighted and the link
// strength in its class colour.
func linkPhraseColoured(fx *db.MSFix) string {
	switch fx.Link {
	case db.LinkClosedBy:
		return fmt.Sprintf("<%s>closed by</> PR <lightCyan>#%d</>", cli.ClassTag(fx.Link), fx.PRNumber)
	case db.LinkLinked:
		return fmt.Sprintf("<%s>linked</> fix PR <lightCyan>#%d</>", cli.ClassTag(fx.Link), fx.PRNumber)
	default:
		return fmt.Sprintf("<%s>mentioned by</> PR <lightCyan>#%d</>", cli.ClassTag(fx.Link), fx.PRNumber)
	}
}

// msReasonColoured is msReason for the console: each fix PR phrase in its class
// colour, the connective text gray. Callers supply the surrounding parens so the
// tags never nest.
func msReasonColoured(fdg *msFinding) string {
	switch {
	case len(fdg.via) > 0:
		phrases := make([]string, 0, len(fdg.via))
		for i := range fdg.via {
			phrases = append(phrases, linkPhraseColoured(&fdg.via[i]))
		}
		return fmt.Sprintf("%s <gray>— in the %s changelog</>", strings.Join(phrases, "<gray>, </>"), fdg.expected)
	case fdg.cited:
		return fmt.Sprintf("<%s>cited directly</> <gray>by the %s changelog</>", cli.ClassTag(cli.LinkCited), fdg.expected)
	default:
		return "<gray>no changelog mapping</>"
	}
}

// msReason explains, for humans, why the expected milestone was determined.
func msReason(fdg *msFinding) string {
	switch {
	case len(fdg.via) > 0:
		phrases := make([]string, 0, len(fdg.via))
		for i := range fdg.via {
			phrases = append(phrases, linkPhrase(&fdg.via[i]))
		}
		return fmt.Sprintf("%s — in the %s changelog", strings.Join(phrases, ", "), fdg.expected)
	case fdg.cited:
		return fmt.Sprintf("the %s changelog cites this issue directly", fdg.expected)
	default:
		return ""
	}
}

// printMissingMilestones calls out releases the changelog proves shipped but
// that have no milestone — nothing can be fixed against them until a human
// creates the milestone, so say so and show how.
func (f *Flags) printMissingMilestones(versions []string) {
	if len(versions) == 0 {
		return
	}
	cout.Printf("\n<red>%d release(s) have no milestone — the findings above wanting them can't be fixed until they exist:</> <lightMagenta>%s</>\n",
		len(versions), strings.Join(versions, " "))
	cout.Printf("  create them (closed) then rerun: <cyan>gh api repos/%s/milestones -f title=<version> -f state=closed</>\n", f.GH.Repo)
}

// applyHint phrases what --apply would do, skipping whichever count is zero,
// e.g. "set the 12 missing milestones", "correct the 6 mismatched PR milestones".
func applyHint(missing, mismatched int, what string) string {
	parts := make([]string, 0, 2)
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("set the %d missing", missing))
	}
	if mismatched > 0 {
		parts = append(parts, fmt.Sprintf("correct the %d mismatched", mismatched))
	}
	return strings.Join(parts, " and ") + " " + what
}

// normalizeMilestone strips the leading v so titles compare against changelog versions.
func normalizeMilestone(title string) string {
	return strings.TrimPrefix(title, "v")
}

func (f *Flags) writeMilestoneCSV(path string, findings []msFinding) error {
	out, err := os.Create(path) //nolint:gosec // G304: user-chosen output path is the point
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = out.Close() }()

	w := csv.NewWriter(out)
	if err := w.Write([]string{cli.CSVColNumber, "bucket", "state", "state_reason", "current_milestone", "expected_milestone", "fix_prs", "reason", cli.CSVColTitle, cli.CSVColURL}); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	for _, fdg := range findings {
		prs := make([]string, 0, len(fdg.fixPRs))
		for _, pr := range fdg.fixPRs {
			prs = append(prs, strconv.Itoa(pr))
		}
		row := []string{
			strconv.Itoa(fdg.issue.Number), fdg.bucket, fdg.issue.State, fdg.issue.StateReason,
			fdg.issue.Milestone, fdg.expected, strings.Join(prs, " "), fdg.reason,
			text.OneLine(fdg.issue.Title),
			f.IssueURL(fdg.issue.Number),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	w.Flush()
	return w.Error()
}

// applyMilestones sets the determined milestone on closed issues — filling
// missing ones and correcting mismatches alike: the changelog is the ground
// truth of what shipped where, so the determined release overwrites a differing
// milestone. CLOSED issues only — an open issue's milestone can be a deliberate
// plan (e.g. a future major), not bookkeeping — so open-released stays
// report-only.
func (f *Flags) applyMilestones(d *db.DB, findings []msFinding, milestones map[string]db.Milestone, o MilestoneOpts) error {
	var todo []msFinding
	for _, fdg := range findings {
		switch {
		case fdg.bucket == msMissing:
			todo = append(todo, fdg)
		case fdg.bucket == msMismatch && fdg.issue.State == db.IssueClosed:
			if _, ok := milestones[fdg.expected]; ok {
				todo = append(todo, fdg)
			}
		}
	}
	if len(todo) == 0 {
		cout.Printf("no determinable missing or mismatched milestones to apply\n")
		return nil
	}

	// --apply-with-ai[-auto] pipelines AI judging with the apply: batch N's
	// candidates are reviewed and set while batch N+1 is already off being scored
	if o.ApplyWithAI || o.ApplyWithAIAuto {
		return f.applyMilestonesWithAI(d, todo, milestones, o)
	}

	// --max caps a real apply into waves; a dry-run previews the complete list
	if !f.DryRun && o.Max > 0 && len(todo) > o.Max {
		cout.Printf("<gray>capping at %d of %d (--max; dry-run shows all)</>\n", o.Max, len(todo))
		todo = todo[:o.Max]
	}

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}

	cout.Printf("setting milestones on <yellow>%d</> issues in %s%s\n", len(todo), f.RepoTag(), issue.DryRunTag(f.DryRun))
	if !f.DryRun && !f.Yes {
		ok, err := issue.Confirm(fmt.Sprintf("set milestones on <yellow>%d</> issues in %s?", len(todo), f.RepoTag()))
		if err != nil {
			return err
		}
		if !ok {
			cout.Printf("aborted\n")
			return nil
		}
	}

	throttle := cli.NewThrottle()
	applied, failed := 0, 0
	for n := range todo {
		res, err := f.applyOneMilestone(d, repo, &todo[n], milestones, n+1, len(todo), throttle, nil, false)
		if err != nil {
			return err
		}
		switch res {
		case issue.ApplySet:
			applied++
		case issue.ApplyFailed:
			failed++
		case issue.ApplyPreviewed:
		}
	}

	if f.DryRun {
		cout.Printf("\n<yellow>dry-run:</> %d milestone sets previewed, nothing changed\n", len(todo))
		cout.Printf("<gray>drop</> <cyan>--dry-run</> <gray>to set these, or switch to</> <cyan>--apply-with-ai</> <gray>to confirm each first</>\n")
		return nil
	}
	cout.Printf("\n<green>%d set</> · %d failed\n", applied, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d milestone sets failed", failed, len(todo))
	}
	return nil
}

// printMSEvidence prints one finding's evidence lines: each fix PR at its link
// strength with the changelog bullet that shipped it, and a direct citation
// when the changelog names the issue itself.
func (f *Flags) printMSEvidence(d *db.DB, fdg *msFinding) error {
	version := normalizeMilestone(fdg.expected)
	for i := range fdg.via {
		fx := &fdg.via[i]
		bullet, err := d.ChangelogTextFor(version, fx.PRNumber)
		if err != nil {
			return err
		}
		cout.Printf("      %s — <lightMagenta>%s</><gray>@changelog:</> %s\n",
			linkPhraseColoured(fx), fdg.expected, text.OrDefault(text.TruncateRunes(changelogBullet(bullet), 100), "<gray>(bullet not found)</>"))
	}
	if fdg.cited {
		bullet, err := d.ChangelogTextFor(version, fdg.issue.Number)
		if err != nil {
			return err
		}
		cout.Printf("      <%s>cited directly</> — <lightMagenta>%s</><gray>@changelog:</> %s\n",
			cli.ClassTag(cli.LinkCited), fdg.expected, text.OrDefault(text.TruncateRunes(changelogBullet(bullet), 100), "<gray>(bullet not found)</>"))
	}
	return nil
}

// applyOneMilestone prints one finding's card — evidence, mismatch callout, AI
// score when judged — and sets the milestone, or previews it under dry-run.
// With ask, the human confirms each set: (a)ccept (s)kip (o)pen (q)uit, with
// y/n as quiet aliases.
func (f *Flags) applyOneMilestone(d *db.DB, repo gh.Repo, fdg *msFinding, milestones map[string]db.Milestone, pos, total int, throttle func(), v *issue.Verdict, ask bool) (int, error) {
	m := milestones[fdg.expected]

	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.TruncateRunes(fdg.issue.Title, 90), f.IssueURL(fdg.issue.Number))
	if err := f.printMSEvidence(d, fdg); err != nil {
		return issue.ApplyFailed, err
	}
	if fdg.bucket == msMismatch {
		cout.Printf("      <yellow>mismatch: carries %s, the changelog says %s</>\n", fdg.issue.Milestone, fdg.expected)
	}
	cli.PrintVerdict(v)

	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would set milestone → %s</> <gray>(and sync it onto the fix PR if missing there)</>\n", fdg.expected)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, err := issue.AskClose(fmt.Sprintf("set → <lightMagenta>%s</>?", fdg.expected), "", f.IssueURL(fdg.issue.Number))
		if err != nil || res != issue.AskAccept {
			return res, err
		}
	}

	throttle()
	if err := repo.SetMilestone(fdg.issue.Number, m.Number); err != nil {
		cout.Errorf("      <red>failed: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	cout.Printf("      <fg=28>set milestone →</> <lightMagenta>%s</>\n", fdg.expected)
	cout.QuietOnlyf("%d@milestone@%s\n", fdg.issue.Number, fdg.expected)
	f.syncFixPRMilestones(repo, fdg, &m, throttle)

	// keep the local scan in sync so a re-audit doesn't re-propose it
	if _, err := d.Exec("UPDATE ms_issues SET milestone = ? WHERE number = ?", fdg.expected, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, fmt.Errorf("updating scan row for #%d: %w", fdg.issue.Number, err)
	}
	return issue.ApplySet, nil
}

// The ms-match judge: every issue↔evidence pairing scored by the AI.
const (
	passMSMatch   = "ms-match"
	promptMSMatch = "milestone-evidence-match"
	// bulletRunesForAI keeps the raw changelog bullet (link refs included — the
	// URL is the number-collision tell) to a sane prompt size.
	bulletRunesForAI = 300
)

// msJudgeItems renders one ms-match judge block per finding, fetching the
// full text of every candidate issue and evidence PR first — the model
// judges on full text.
func (f *Flags) msJudgeItems(d *db.DB, todo []msFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptMSMatch)
	if err != nil {
		return "", nil, err
	}
	if err := f.fetchMatchTexts(d, todo); err != nil {
		return "", nil, err
	}
	texts, err := d.Texts()
	if err != nil {
		return "", nil, err
	}
	items := make([]issue.JudgeItem, 0, len(todo))
	for i := range todo {
		block, berr := f.msMatchBlock(d, &todo[i], texts)
		if berr != nil {
			return "", nil, berr
		}
		items = append(items, issue.JudgeItem{Number: todo[i].issue.Number, Block: block})
	}
	return promptText, items, nil
}

// applyMilestonesWithAI is --apply-with-ai[-auto], pipelined on the shared
// judge: every issue↔evidence pairing is scored by the AI CLI, and only likely
// matches (≥ threshold) get their milestone set. While batch N's results are
// reviewed and applied, batch N+1 is already off being scored in the
// background, and auto mode's confirmation prompt comes right after batch 1 so
// answer time overlaps judging too. Verdicts cache in ai_verdicts so re-runs
// (and the real apply after a dry-run) only judge what changed.
func (f *Flags) applyMilestonesWithAI(d *db.DB, todo []msFinding, milestones map[string]db.Milestone, o MilestoneOpts) error {
	promptText, items, err := f.msJudgeItems(d, todo)
	if err != nil {
		return err
	}
	byNumber := map[int]*msFinding{}
	for i := range todo {
		byNumber[todo[i].issue.Number] = &todo[i]
	}

	auto := o.ApplyWithAIAuto
	threshold := o.Threshold
	if threshold <= 0 {
		threshold = cli.JudgeThreshold
	}
	// interactive: the AI's score advises, the human confirms every set
	interactive := !auto && !f.DryRun

	mode := "<gray>you confirm each set</>"
	switch {
	case f.DryRun:
		mode = fmt.Sprintf("<gray>previewing the ≥</> <green>%.2f</> <gray>gate</>", threshold)
	case auto:
		mode = fmt.Sprintf("<gray>auto-applying ≥</> <green>%.2f</>", threshold)
	}
	cout.Printf("AI match check on <yellow>%d</> candidates in %s <gray>·</> %s\n", len(todo), f.RepoTag(), mode)

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}
	throttle := cli.NewThrottle()

	pos, applied, failed, previewed, below, unanswered, humanSkipped := 0, 0, 0, 0, 0, 0, 0
	// process gates and applies one slice of judged targets; interactive mode
	// shows every scored candidate and asks, auto/dry-run gate on the threshold
	process := func(ts []issue.Judged) (bool, error) {
		for _, t := range ts {
			pos++
			fdg, v := byNumber[t.Number], t.Verdict
			switch {
			case v == nil:
				unanswered++
				cout.Printf("\n  <gray>%d/%d</> <gray>skip</> <cyan>#%d</> <yellow>no verdict</> %s\n",
					pos, len(todo), fdg.issue.Number, text.TruncateRunes(fdg.issue.Title, 70))
			case !interactive && v.Confidence < threshold:
				below++
				cout.Printf("\n  <gray>%d/%d</> <gray>skip</> <cyan>#%d</> <%s>%.2f</> %s <darkGray>%s</>\n",
					pos, len(todo), fdg.issue.Number, cli.ScoreTag(v.Confidence), v.Confidence,
					text.TruncateRunes(fdg.issue.Title, 80), f.IssueURL(fdg.issue.Number))
				cout.Printf("        <lightWhite>%s</>\n", text.OneLine(v.Reason))
			default:
				res, aerr := f.applyOneMilestone(d, repo, fdg, milestones, pos, len(todo), throttle, v, interactive)
				if aerr != nil {
					return true, aerr
				}
				switch res {
				case issue.ApplySet:
					applied++
				case issue.ApplyFailed:
					failed++
				case issue.ApplyPreviewed:
					previewed++
				case issue.ApplySkipped:
					humanSkipped++
				case issue.ApplyQuit:
					cout.Printf("<gray>quitting — %d candidates left unreviewed</>\n", len(todo)-pos)
					return true, nil
				}
				if !f.DryRun && o.Max > 0 && applied >= o.Max {
					cout.Printf("<gray>--max reached: %d applied, skipping the rest (dry-run shows all)</>\n", o.Max)
					return true, nil
				}
			}
		}
		return false, nil
	}
	// interactive mode asks per item; the up-front confirm is auto mode's gate
	onReady := func() (bool, error) {
		if !auto || f.DryRun || f.Yes {
			return true, nil
		}
		ok, cerr := issue.Confirm(fmt.Sprintf("auto-apply milestone sets the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", threshold, len(todo), f.RepoTag()))
		if cerr == nil && !ok {
			cout.Printf("aborted\n")
		}
		return ok, cerr
	}

	if _, err := f.JudgeBlocks(d, passMSMatch, promptText, items, onReady, process); err != nil {
		return err
	}

	if interactive {
		cout.Printf("\n<green>%d set</> · %d skipped by you · %d failed · <yellow>%d</> unanswered\n",
			applied, humanSkipped, failed, unanswered)
	} else {
		cout.Printf("\nAI match gate: <green>%d</> at or above %.2f · <fg=208>%d</> below · <yellow>%d</> unanswered\n",
			applied+failed+previewed, threshold, below, unanswered)
		if f.DryRun {
			cout.Printf("<yellow>dry-run:</> %d milestone sets previewed, nothing changed\n", previewed)
			return nil
		}
		cout.Printf("<green>%d set</> · %d failed\n", applied, failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d milestone sets failed", failed)
	}
	return nil
}

// msMatchBlock renders one finding for the ms-match prompt: the issue (title +
// full body) and every piece of changelog evidence behind its determined
// milestone — each evidence PR's title and body alongside its raw changelog
// bullet, so the AI compares actual content, not just names. Bullets keep
// their link refs: the URL is the number-collision tell.
func (f *Flags) msMatchBlock(d *db.DB, fdg *msFinding, texts map[int]db.Text) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.issue.Number, text.OneLine(fdg.issue.Title))
	fmt.Fprintf(&b, "determined milestone: %s\n", fdg.expected)
	if t, ok := texts[fdg.issue.Number]; ok && t.Body != "" {
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(t.Body), cli.IssueBodyRunes))
	}

	b.WriteString("EVIDENCE:\n")
	version := normalizeMilestone(fdg.expected)
	for i := range fdg.via {
		fx := &fdg.via[i]
		bullet, err := d.ChangelogTextFor(version, fx.PRNumber)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- %s — %s changelog: %s\n", linkPhrase(fx), fdg.expected,
			text.OrDefault(text.TruncateRunes(text.OneLine(bullet), bulletRunesForAI), "(bullet not found)"))
		if t, ok := texts[fx.PRNumber]; ok && t.Title != "" {
			fmt.Fprintf(&b, "  PR #%d (%s): %s\n", fx.PRNumber, strings.ToLower(t.State), text.OneLine(t.Title))
			if t.Body != "" {
				fmt.Fprintf(&b, "  PR BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(t.Body), cli.PRBodyRunes))
			}
		}
	}
	if fdg.cited {
		bullet, err := d.ChangelogTextFor(version, fdg.issue.Number)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- the %s changelog cites #%d directly: %s\n", fdg.expected, fdg.issue.Number,
			text.OrDefault(text.TruncateRunes(text.OneLine(bullet), bulletRunesForAI), "(bullet not found)"))
	}
	return b.String(), nil
}

// fetchMatchTexts fills the texts cache for every candidate issue and evidence
// PR behind the findings.
func (f *Flags) fetchMatchTexts(d *db.DB, todo []msFinding) error {
	numbers := map[int]bool{}
	for i := range todo {
		fdg := &todo[i]
		numbers[fdg.issue.Number] = true
		for _, fx := range fdg.via {
			numbers[fx.PRNumber] = true
		}
	}
	return f.FetchTexts(d, text.SortedKeys(numbers))
}

// ---- report ----

// msBucketMeta is what the report says about each bucket.
var msBucketMeta = map[string]struct{ question, description, command string }{
	msMissing: {
		"this closed issue has no milestone, but the changelog says which release dealt with it — fill it in?",
		"Closed issues with no milestone whose fix PRs map to a release via the changelog. The evidence class split is the wave plan: closed-by is near-certain, mention needs the AI.",
		"koi milestone --skip-scan --apply / --apply-with-ai / --apply-with-ai-auto",
	},
	msMismatch: {
		"this issue's milestone differs from the release the changelog determined — correct it?",
		"Issues whose existing milestone disagrees with the changelog-determined release. The changelog is the ground truth of what shipped where, so --apply corrects these.",
		"koi milestone --skip-scan --apply / --apply-with-ai / --apply-with-ai-auto",
	},
	msNoSuchRelease: {
		"the determined release has no milestone in the repo — create it to fix these",
		"The changelog names the shipping release but no matching milestone exists on GitHub. Creating the milestone (gh api) unblocks the apply.",
		"gh api repos/:owner/:repo/milestones -f title=vX.Y.Z -f state=closed, then koi milestone --skip-scan --apply",
	},
	msOpenReleased: {
		"this issue is OPEN but sits on an already-released milestone — reopen the question of why",
		"Open issues carrying a milestone that has shipped: either the fix landed and the issue should close, or the milestone is wrong. Report-only; nothing applies here.",
		"koi close fixed (the fix may have shipped) or manual review",
	},
}

// msClassKind maps an evidence class to its report css kind, strongest to
// weakest: ok, mid, warn, dim.
func msClassKind(class string) string {
	switch class {
	case db.LinkClosedBy:
		return cli.KindOK
	case db.LinkLinked:
		return cli.KindMid
	case cli.LinkCited:
		return cli.KindWarn
	default:
		return cli.KindDim
	}
}

// Report writes milestone-<stamp>.html: the audit's findings bucket by bucket with
// linked evidence, riding the shared report scaffolding. --with-ai scores the
// actionable buckets (missing, mismatch) with the ms-match judge.
func (f *Flags) Report() error {
	o := f.Cmd.Report
	if o.WithAI {
		if !f.AI.Enabled {
			return errors.New("--with-ai needs the AI (--ai=false is set)")
		}
		if err := f.RequireAI(); err != nil {
			return err
		}
	}

	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	if !f.Cmd.MS.SkipScan && !f.NoAutoFetch {
		if err := f.MilestoneScan(d, f.Cmd.MS.Rescan); err != nil {
			return err
		}
	}
	col, err := f.collectMilestone(d, "")
	if err != nil {
		return err
	}
	if col.total == 0 {
		cout.Printf("no scanned issues — run <cyan>koi milestone</> without --skip-scan first\n")
		return nil
	}

	now := time.Now()
	data := cli.ReportData{Repo: f.GH.Repo, Noun: "milestone findings", WithAI: o.WithAI, GeneratedAt: now.Format("2006-01-02 15:04")}

	// the AI scores only the actionable buckets, after the per-bucket limit
	byBucket := map[string][]msFinding{}
	for _, fdg := range col.findings {
		byBucket[fdg.bucket] = append(byBucket[fdg.bucket], fdg)
	}
	truncated := map[string]bool{}
	for b := range byBucket {
		byBucket[b], truncated[b] = cli.LimitFindings(byBucket[b], o.Limit)
	}
	var verdicts map[int]*issue.Verdict
	if o.WithAI {
		todo := append(append([]msFinding{}, byBucket[msMissing]...), byBucket[msMismatch]...)
		if len(todo) > 0 {
			promptText, items, jerr := f.msJudgeItems(d, todo)
			if jerr != nil {
				return jerr
			}
			if verdicts, err = f.JudgeBlocks(d, passMSMatch, promptText, items, nil, nil); err != nil {
				return err
			}
		}
	}

	for _, bucket := range []string{msMissing, msMismatch, msNoSuchRelease, msOpenReleased} {
		findings := byBucket[bucket]
		if col.counts[bucket] == 0 {
			continue
		}
		if verdicts != nil && (bucket == msMissing || bucket == msMismatch) {
			cli.SortByVerdict(findings, func(x *msFinding) int { return x.issue.Number }, verdicts)
		}
		meta := msBucketMeta[bucket]
		s := cli.ReportSection{
			Slug: "ms-" + bucket, Name: "milestone · " + bucket,
			Question: meta.question, Description: meta.description, Command: meta.command,
			Total: col.counts[bucket], Truncated: truncated[bucket],
		}
		for _, class := range []string{db.LinkClosedBy, db.LinkLinked, cli.LinkCited, db.LinkMention} {
			if n := col.classCounts[bucket][class]; n > 0 {
				s.Classes = append(s.Classes, cli.ReportClass{Name: class, Count: n, Kind: msClassKind(class)})
			}
		}
		if bucket == msMissing {
			s.Note = fmt.Sprintf("audit over %d issues · %d ok · %d undetermined (closed as completed, no changelog-mapped fix)",
				col.total, col.counts[msOK], col.counts[msUndetermined])
		}
		for i := range findings {
			fdg := &findings[i]
			item := cli.ReportItem{
				Number: fdg.issue.Number, Title: text.OneLine(fdg.issue.Title),
				URL:  f.IssueURL(fdg.issue.Number),
				Meta: strings.ToLower(fdg.issue.State),
			}
			if !fdg.issue.ClosedAt.IsZero() {
				item.Meta += " · closed " + fdg.issue.ClosedAt.Format("2006-01-02")
			}
			row := []cli.ReportSpan{cli.Span("milestone", cli.KindDim)}
			cur := fdg.issue.Milestone
			switch {
			case cur == "":
				row = append(row, cli.Span("—", cli.KindDim))
			case bucket == msMismatch:
				row = append(row, cli.Span(cur, cli.KindBad))
			default:
				row = append(row, cli.Span(cur, cli.KindWarn))
			}
			if fdg.expected != "" && fdg.expected != cur {
				row = append(row, cli.Span("→", cli.KindDim), cli.Span(fdg.expected, cli.KindVer))
			}
			if fdg.noMilestone {
				row = append(row, cli.Span("no such milestone — create it to fix", cli.KindBad))
			}
			item.Evidence = append(item.Evidence, row)
			if fdg.reason != "" {
				ev := []cli.ReportSpan{cli.Span(fdg.reason, msClassKind(fdg.linkClass()))}
				for _, fx := range fdg.via {
					ev = append(ev, cli.LinkSpan(fmt.Sprintf("PR #%d", fx.PRNumber),
						fmt.Sprintf("https://github.com/%s/pull/%d", f.GH.Repo, fx.PRNumber)))
				}
				item.Evidence = append(item.Evidence, ev)
			}
			cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
			s.Items = append(s.Items, item)
		}
		data.Sections = append(data.Sections, s)
		data.Total += s.Total
	}

	if err := os.MkdirAll(o.Out, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", o.Out, err)
	}
	htmlPath := filepath.Join(o.Out, cli.ReportFileName("milestone", now))
	if err := cli.WriteReportHTML(htmlPath, &data); err != nil {
		return err
	}
	cout.Printf("\nwrote <cyan>%s</> — <yellow>%d</> milestone findings <gray>(missing %d · mismatch %d · no-milestone %d · open-released %d)</>\n",
		htmlPath, data.Total, col.counts[msMissing], col.counts[msMismatch], col.counts[msNoSuchRelease], col.counts[msOpenReleased])
	if !o.WithAI {
		cout.Printf("<gray>rerun with</> <cyan>--with-ai</> <gray>to score the actionable buckets, or</> <cyan>--limit 10</> <gray>to test cheaply</>\n")
	}
	// a file:// url so the terminal makes the path clickable
	if abs, aerr := filepath.Abs(htmlPath); aerr == nil {
		cout.Printf("<gray>open:</> <cyan>file://%s</>\n", abs)
	}
	return nil
}
