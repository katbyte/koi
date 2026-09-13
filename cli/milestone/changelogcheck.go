package milestone

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// ChangelogCheck audits the PR side of milestone bookkeeping: every changelog
// bullet cites the PR that shipped in that release, so that PR should carry the
// release's milestone. This inverts the issue audit — the changelog is the
// ground truth of what shipped where, and it covers exactly the PRs that need
// milestones without walking the repository's full PR history.
func (f *Flags) ChangelogCheck() error {
	o := MilestoneOpts{FlagsMilestone: f.Cmd.MS, FlagsApplyModes: f.Modes}
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	prVersions, err := d.ChangelogVersionsByPR()
	if err != nil {
		return err
	}
	if len(prVersions) == 0 {
		cout.Printf("changelog table is empty — run <cyan>koi fetch</> first\n")
		return nil
	}

	cache, err := d.MSPRs()
	if err != nil {
		return err
	}
	if err := f.fetchChangelogPRs(d, prVersions, cache, o.Rescan); err != nil {
		return err
	}
	// re-read: fetchChangelogPRs grew the cache
	if cache, err = d.MSPRs(); err != nil {
		return err
	}
	milestones, err := d.Milestones()
	if err != nil {
		return err
	}

	// audit every changelog-cited PR against the release(s) that cite it
	counts := map[string]int{}
	var findings []prFinding
	unknown := 0
	for _, pr := range text.SortedKeys(prVersions) {
		cached, ok := cache[pr]
		if !ok {
			unknown++ // not fetched (e.g. --no-auto-fetch with a cold cache)
			continue
		}
		if !cached.IsPR {
			continue // a changelog bullet citing an issue — nothing to check
		}
		fdg := auditChangelogPR(&cached, prVersions[pr], milestones)
		counts[fdg.bucket]++
		if fdg.bucket != msOK {
			findings = append(findings, fdg)
		}
	}

	cout.Printf("\n<bold>changelog check over %d cited PRs:</>\n", len(prVersions))
	for _, k := range text.SortedKeys(counts) {
		cout.Printf("  %-16s <yellow>%d</>\n", k, counts[k])
	}
	if unknown > 0 {
		cout.Printf("  %-16s <yellow>%d</> <gray>(not in the local cache — rerun without --no-auto-fetch)</>\n", "unfetched", unknown)
	}

	shown := map[string]int{}
	for i := range findings {
		fdg := &findings[i]
		if o.Bucket != "" && fdg.bucket != o.Bucket {
			continue
		}
		if o.Bucket == "" {
			if shown[fdg.bucket] >= 10 {
				continue
			}
			shown[fdg.bucket]++
		}
		current := fdg.pr.Milestone
		if current == "" {
			current = "(none)"
		}
		target, note := cli.OrDash(fdg.expected), ""
		if fdg.expected == "" && fdg.wanted != "" {
			target = fdg.wanted
			note = fmt.Sprintf(" <red>no %s milestone — create it to fix</>", fdg.wanted)
		}
		cout.Printf("  <gray>%-14s</> <cyan>#%d</> %s → <lightMagenta>%s</> <gray>(cited in the %s changelog)</>%s %s <darkGray>%s</>\n",
			fdg.bucket, fdg.pr.Number, current, target, joinVersions(fdg.versions), note,
			text.TruncateRunes(fdg.pr.Title, 60), f.PRURL(fdg.pr.Number))
	}
	if o.Bucket == "" && len(findings) > 10 {
		cout.Printf("<gray>(showing up to 10 per bucket — use</> <cyan>--csv</> <gray>for the full list,</> <cyan>--bucket <name></> <gray>for one bucket)</>\n")
	}

	if o.CSV != "" {
		if err := f.writeChangelogCheckCSV(o.CSV, findings); err != nil {
			return err
		}
		cout.Printf("wrote <cyan>%s</> (%d findings)\n", o.CSV, len(findings))
	}

	f.printMissingMilestones(prFindingWants(findings))

	if o.Apply {
		return f.applyPRMilestones(d, findings, milestones, o)
	}
	if counts[msMissing]+counts[msMismatch] > 0 {
		cout.Printf("next: <cyan>koi milestone changelog-check --apply --dry-run</> to preview, then drop <cyan>--dry-run</> to %s\n", applyHint(counts[msMissing], counts[msMismatch], "PR milestones"))
	}
	return nil
}

// prFinding is one changelog-check result for a PR.
type prFinding struct {
	pr       db.MSPR
	bucket   string   // msMissing | msMismatch | msNoSuchRelease | msOK
	expected string   // milestone title, e.g. "v4.80.0" ("" when none of the citing releases has one)
	wanted   string   // the earliest citing release, milestone or not — what expected would be if it existed
	versions []string // every release whose changelog cites this PR
}

// auditChangelogPR buckets one PR against the releases citing it. A milestone
// matching ANY citing release is ok (backports are citations in two changelogs);
// the earliest citing release with an existing milestone fills a missing one.
func auditChangelogPR(pr *db.MSPR, versions []string, milestones map[string]db.Milestone) prFinding {
	fdg := prFinding{pr: *pr, bucket: msOK, versions: versions}

	cited := map[string]bool{}
	expected, wanted := "", ""
	for _, v := range versions {
		cited[v] = true
		if wanted == "" || issue.VersionLess(v, wanted) {
			wanted = v
		}
		if _, ok := milestones["v"+v]; ok && (expected == "" || issue.VersionLess(v, expected)) {
			expected = v
		}
	}
	if expected != "" {
		fdg.expected = "v" + expected
	}
	if wanted != "" {
		fdg.wanted = "v" + wanted
	}

	switch {
	case pr.Milestone == "" && expected != "":
		fdg.bucket = msMissing
	case pr.Milestone == "":
		fdg.bucket = msNoSuchRelease
	case !cited[normalizeMilestone(pr.Milestone)]:
		fdg.bucket = msMismatch
	}
	return fdg
}

// fetchChangelogPRs fills the PR cache for every changelog-cited number not yet
// known (all of them with rescan), 50 per aliased query.
func (f *Flags) fetchChangelogPRs(d *db.DB, prVersions map[int][]string, cache map[int]db.MSPR, rescan bool) error {
	var need []int
	for _, pr := range text.SortedKeys(prVersions) {
		if _, ok := cache[pr]; !ok || rescan {
			need = append(need, pr)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if f.NoAutoFetch {
		cout.Printf("<yellow>%d cited PRs not in the local cache and --no-auto-fetch is set — auditing what's cached</>\n", len(need))
		return nil
	}

	owner, name, err := f.RepoOwnerName()
	if err != nil {
		return err
	}
	client := f.NewGraphQL()
	cout.Printf("fetching milestones for <yellow>%d</> changelog-cited PRs from <white>%s</>/<cyan>%s</>...\n", len(need), owner, name)

	for start := 0; start < len(need); start += 50 {
		batch := need[start:min(start+50, len(need))]
		nodes, rl, err := client.PRMilestones(owner, name, batch)
		if err != nil {
			return err
		}

		prs := make([]db.MSPR, 0, len(batch))
		for _, n := range batch {
			node := nodes[n]
			if node == nil {
				prs = append(prs, db.MSPR{Number: n, IsPR: false})
				continue
			}
			prs = append(prs, db.MSPR{Number: n, Title: node.Title, State: node.State, Milestone: node.Milestone.Title, IsPR: true})
		}
		if err := d.SaveMSPRs(prs); err != nil {
			return err
		}

		for i, p := range prs {
			printCheckedPR(start+i+1, len(need), &p)
		}
		cout.Printf("  <gray>%d/%d fetched · rate limit: %d remaining</>\n", start+len(batch), len(need), rl.Remaining)
		rl.WaitIfLow()
	}
	return nil
}

// printCheckedPR is one line per fetched PR: number, state, milestone, title.
func printCheckedPR(pos, total int, p *db.MSPR) {
	if !p.IsPR {
		cout.Printf("  <gray>%6d/%d</> <cyan>#%-6d</> <gray>cited number is an issue, skipping</>\n", pos, total, p.Number)
		return
	}
	state := text.StateTag(p.State)
	ms := "<red>no milestone</>"
	if p.Milestone != "" {
		ms = "<lightMagenta>" + p.Milestone + "</>"
	}
	cout.Printf("  <gray>%6d/%d</> <cyan>#%-6d</> %s %s <gray>·</> %s\n", pos, total, p.Number, state, ms, text.TruncateRunes(text.OneLine(p.Title), 60))
}

// applyPRMilestones sets the citing release's milestone on changelog-cited PRs —
// filling missing ones and correcting mismatches alike: the changelog is the
// ground truth of what shipped where, so the citing release overwrites a
// differing milestone. MERGED PRs only, and only where a citing release has a
// milestone to point at.
func (f *Flags) applyPRMilestones(d *db.DB, findings []prFinding, milestones map[string]db.Milestone, o MilestoneOpts) error {
	var todo []prFinding
	for _, fdg := range findings {
		switch {
		case fdg.bucket == msMissing:
			todo = append(todo, fdg)
		case fdg.bucket == msMismatch && fdg.pr.State == db.PRMerged && fdg.expected != "":
			todo = append(todo, fdg)
		}
	}
	if len(todo) == 0 {
		cout.Printf("no missing or mismatched PR milestones to apply\n")
		return nil
	}
	if !f.DryRun && o.Max > 0 && len(todo) > o.Max {
		cout.Printf("<gray>capping at %d of %d (--max; dry-run shows all)</>\n", o.Max, len(todo))
		todo = todo[:o.Max]
	}

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}

	cout.Printf("setting milestones on <yellow>%d</> PRs in %s%s\n", len(todo), f.RepoTag(), issue.DryRunTag(f.DryRun))
	if !f.DryRun && !f.Yes {
		ok, err := issue.Confirm(fmt.Sprintf("set milestones on <yellow>%d</> PRs in %s?", len(todo), f.RepoTag()))
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
	for n, fdg := range todo {
		version := normalizeMilestone(fdg.expected)
		cout.Printf("  <gray>%d/%d</> <cyan>#%d</> <bold>%s</> <darkGray>%s</>\n",
			n+1, len(todo), fdg.pr.Number, text.TruncateRunes(fdg.pr.Title, 90), f.PRURL(fdg.pr.Number))
		bullet, err := d.ChangelogTextFor(version, fdg.pr.Number)
		if err != nil {
			return err
		}
		cout.Printf("      cited in <lightMagenta>%s</><gray>@changelog:</> %s\n",
			fdg.expected, text.OrDefault(text.TruncateRunes(changelogBullet(bullet), 100), "<gray>(bullet not found)</>"))
		if fdg.bucket == msMismatch {
			cout.Printf("      <yellow>mismatch: carries %s, the changelog says %s</>\n", fdg.pr.Milestone, fdg.expected)
		}

		if f.DryRun {
			cout.Printf("      <yellow>dry-run: would set milestone → %s</>\n", fdg.expected)
			continue
		}

		throttle()
		if err := repo.SetMilestone(fdg.pr.Number, milestones[fdg.expected].Number); err != nil {
			cout.Errorf("      <red>failed: %v</>\n", err)
			failed++
			continue
		}
		applied++
		cout.Printf("      <fg=28>set milestone →</> <lightMagenta>%s</>\n", fdg.expected)
		cout.QuietOnlyf("%d@pr-milestone@%s\n", fdg.pr.Number, fdg.expected)
		if err := d.SetMSPRMilestone(fdg.pr.Number, fdg.expected); err != nil {
			return err
		}
	}

	if f.DryRun {
		cout.Printf("\n<yellow>dry-run:</> %d PR milestone sets previewed, nothing changed\n", len(todo))
		return nil
	}
	cout.Printf("\n<green>%d set</> · %d failed\n", applied, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d PR milestone sets failed", failed, len(todo))
	}
	return nil
}

func (f *Flags) writeChangelogCheckCSV(path string, findings []prFinding) error {
	out, err := os.Create(path) //nolint:gosec // G304: user-chosen output path is the point
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = out.Close() }()

	w := csv.NewWriter(out)
	if err := w.Write([]string{cli.CSVColNumber, "bucket", "state", "current_milestone", "expected_milestone", "cited_in", cli.CSVColTitle, cli.CSVColURL}); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	for i := range findings {
		fdg := &findings[i]
		row := []string{
			strconv.Itoa(fdg.pr.Number), fdg.bucket, fdg.pr.State, fdg.pr.Milestone, fdg.expected,
			joinVersions(fdg.versions), text.OneLine(fdg.pr.Title), f.PRURL(fdg.pr.Number),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	w.Flush()
	return w.Error()
}

// prFindingWants collects the releases whose absent milestone blocks a fix —
// every finding stuck on a wanted release with no milestone to point at.
func prFindingWants(findings []prFinding) []string {
	seen := map[string]bool{}
	for i := range findings {
		if fdg := &findings[i]; fdg.expected == "" && fdg.wanted != "" {
			seen[fdg.wanted] = true
		}
	}
	return text.SortedKeys(seen)
}

func joinVersions(vs []string) string {
	var out strings.Builder
	for i, v := range vs {
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteByte('v')
		out.WriteString(v)
	}
	return out.String()
}
