// The all-issue milestone scan: light rows for every issue (open and closed)
// with their fix-PR links — fetch infrastructure the milestone audit reads.

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/text"
)

const (
	metaMSScanCursor  = "ms_scan_cursor"
	metaMSLastScan    = "ms_last_scan"
	metaMSScanStarted = "ms_scan_started" // when the (possibly resumed) full walk began

)

func (f *FlagData) MilestoneScan(d *db.DB, rescan bool) error {
	owner, name, err := f.RepoOwnerName()
	if err != nil {
		return err
	}
	client := f.NewGraphQL()
	started := db.Now()

	milestones, err := client.AllMilestones(owner, name)
	if err != nil {
		return err
	}
	dbMilestones := make([]db.Milestone, 0, len(milestones))
	for _, m := range milestones {
		dbMilestones = append(dbMilestones, db.Milestone{Number: m.Number, Title: m.Title, State: m.State})
	}
	if err := d.ReplaceMilestones(dbMilestones); err != nil {
		return err
	}
	cout.Printf("fetched <yellow>%d</> milestones from <white>%s</>/<cyan>%s</>\n", len(milestones), owner, name)

	if rescan {
		if err := d.DeleteMeta(metaMSScanCursor); err != nil {
			return err
		}
		if err := d.DeleteMeta(metaMSLastScan); err != nil {
			return err
		}
		if err := d.DeleteMeta(metaMSScanStarted); err != nil {
			return err
		}
	}

	cursor, err := d.GetMeta(metaMSScanCursor)
	if err != nil {
		return err
	}
	lastScan, err := d.GetMeta(metaMSLastScan)
	if err != nil {
		return err
	}

	// the completion stamp must be the time the full walk STARTED — a resumed
	// walk never re-fetches the pages the interrupted run already saved, so a
	// change landing between the original start and the resume is only caught
	// by the next incremental scan if the stamp doesn't move past it
	stamp := started
	switch {
	case lastScan == "" || cursor != "":
		if cursor != "" {
			cout.Printf("<yellow>resuming</> interrupted scan of all issues in <white>%s</>/<cyan>%s</>...\n", owner, name)
			orig, gerr := d.GetMeta(metaMSScanStarted)
			if gerr != nil {
				return gerr
			}
			// no recorded start (scan interrupted before this key existed)
			// falls back to the resume's start — the old, slightly-lossy stamp
			if t, terr := time.Parse(time.RFC3339, orig); terr == nil {
				stamp = t
			}
		} else {
			cout.Printf("scanning ALL issues (open and closed, light fields) in <white>%s</>/<cyan>%s</>...\n", owner, name)
			if err := d.SetMeta(metaMSScanStarted, started.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if err := f.msFullScan(d, client, owner, name, cursor); err != nil {
			return err
		}
	default:
		since, terr := time.Parse(time.RFC3339, lastScan)
		if terr != nil {
			return fmt.Errorf("unparseable ms_last_scan %q — run with --rescan: %w", lastScan, terr)
		}
		since = since.Add(-2 * time.Minute)
		cout.Printf("scanning issues in <white>%s</>/<cyan>%s</> updated since <yellow>%s</>...\n", owner, name, since.Format("2006-01-02 15:04"))
		if err := f.msIncrementalScan(d, client, owner, name, since); err != nil {
			return err
		}
	}

	if err := d.SetMeta(metaMSLastScan, stamp.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := d.DeleteMeta(metaMSScanStarted); err != nil {
		return err
	}
	return d.DeleteMeta(metaMSScanCursor)
}

func (f *FlagData) msFullScan(d *db.DB, client *gh.Client, owner, name, cursor string) error {
	fetched, page := 0, 0
	for {
		p, err := client.ScanIssues(owner, name, cursor)
		if err != nil {
			return err
		}

		bundles := scanBundles(p.Issues, f.GH.Repo)
		if err := d.SaveMSIssues(bundles, metaMSScanCursor, p.PageInfo.EndCursor); err != nil {
			return err
		}

		for i := range bundles {
			printScannedIssue(fetched+i+1, p.TotalCount, &bundles[i])
		}
		fetched += len(p.Issues)
		page++
		if page%5 == 0 || !p.PageInfo.HasNextPage {
			cout.Printf("  <gray>%d/%d scanned · rate limit: %d remaining</>\n", fetched, p.TotalCount, p.RateLimit.Remaining)
		}
		p.RateLimit.WaitIfLow()

		if !p.PageInfo.HasNextPage {
			return nil
		}
		cursor = p.PageInfo.EndCursor
	}
}

func (f *FlagData) msIncrementalScan(d *db.DB, client *gh.Client, owner, name string, since time.Time) error {
	cursor, fetched := "", 0
	for {
		p, err := client.ScanUpdatedIssues(owner, name, since, cursor)
		if err != nil {
			return err
		}

		// the search API silently caps at 1000 results; fall back to a full walk
		if p.TotalCount > 950 {
			cout.Printf("<yellow>%d issues changed since last scan — falling back to a full walk</>\n", p.TotalCount)
			return f.msFullScan(d, client, owner, name, "")
		}

		bundles := scanBundles(p.Issues, f.GH.Repo)
		if err := d.SaveMSIssues(bundles, "", ""); err != nil {
			return err
		}

		for i := range bundles {
			printScannedIssue(fetched+i+1, p.TotalCount, &bundles[i])
		}
		fetched += len(p.Issues)
		p.RateLimit.WaitIfLow()

		if !p.PageInfo.HasNextPage {
			return nil
		}
		cursor = p.PageInfo.EndCursor
	}
}

func scanBundles(nodes []gh.ScanIssueNode, repo string) []db.MSBundle {
	bundles := make([]db.MSBundle, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		b := db.MSBundle{Issue: db.MSIssue{
			Number:      n.Number,
			Title:       n.Title,
			State:       n.State,
			StateReason: n.StateReason,
			Milestone:   n.Milestone.Title,
			ClosedAt:    n.ClosedAt,
			UpdatedAt:   n.UpdatedAt,
		}}
		// one fix row per PR at its strongest link: closed-by (the ClosedEvent's
		// closer) beats linked (closing-keyword reference) beats mention
		best := map[int]db.MSFix{}
		consider := func(s *gh.ScanPRRef, link string) {
			// only merged PRs from the same repo can map to a release
			if s.Typename != TypePullRequest || !s.Merged || !strings.EqualFold(s.Repository.NameWithOwner, repo) {
				return
			}
			if prev, ok := best[s.Number]; ok && db.LinkRank[prev.Link] >= db.LinkRank[link] {
				return
			}
			best[s.Number] = db.MSFix{IssueNumber: n.Number, PRNumber: s.Number, MergedAt: s.MergedAt, Link: link}
		}
		for _, t := range n.TimelineItems.Nodes {
			consider(&t.Closer, db.LinkClosedBy)
			link := db.LinkMention
			if t.WillCloseTarget {
				link = db.LinkLinked
			}
			consider(&t.Source, link)
		}
		for _, pr := range text.SortedKeys(best) {
			b.Fixes = append(b.Fixes, best[pr])
		}
		bundles = append(bundles, b)
	}
	return bundles
}

// printScannedIssue is one line per scanned issue: position, number, state
// (green closed / orange open), title, milestone, and how its fix PRs link.
func printScannedIssue(pos, total int, b *db.MSBundle) {
	i := &b.Issue
	state := text.StateTag(i.State)
	var extra strings.Builder
	if i.Milestone != "" {
		fmt.Fprintf(&extra, " <gray>·</> <lightMagenta>%s</>", i.Milestone)
	}
	byLink := map[string]int{}
	for _, fx := range b.Fixes {
		byLink[fx.Link]++
	}
	for _, link := range []string{db.LinkClosedBy, db.LinkLinked, db.LinkMention} {
		if n := byLink[link]; n > 0 {
			fmt.Fprintf(&extra, " <gray>·</> <%s>%d %s PR(s)</>", ClassTag(link), n, link)
		}
	}
	cout.Printf("  <gray>%6d/%d</> <cyan>#%-6d</> %s %s%s\n",
		pos, total, i.Number, state, text.TruncateRunes(text.OneLine(i.Title), 65), extra.String())
}
