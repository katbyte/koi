package cli

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

const (
	metaFetchCursor   = "fetch_cursor"
	MetaLastSync      = "last_sync"
	metaLastReconcile = "last_reconcile"

	// reconcileEvery is how stale the open-set verification may get before a
	// fetch redoes it. It exists to catch closes the search-index lag swallowed
	// — a rare, slow drift — so back-to-back auto-fetches skip the ~33 requests.
	reconcileEvery = 15 * time.Minute

	// staticsEvery is how long the release-cadence artefacts (changelogs,
	// upgrade guides, provider docs, the milestone scan) stay fresh for the
	// checks' auto-fetch — they cost ~10 requests plus their throttle gaps
	// every run while changing roughly weekly. An explicit koi fetch always
	// refreshes them.
	staticsEvery    = 30 * time.Minute
	metaStaticsSync = "statics_sync"
)

// AutoFetch is the freshness pass every check runs before consuming the db: a
// normal Fetch, except artefacts on release cadence are skipped when they
// were refreshed within staticsEvery.
func (f *FlagData) AutoFetch() error {
	f.staticsTTL = staticsEvery
	defer func() { f.staticsTTL = 0 }()
	return f.Fetch(false)
}

// staticsFresh reports whether the release-cadence artefacts were refreshed
// within the auto-fetch TTL. Explicit fetches (no TTL set) never skip.
func (f *FlagData) staticsFresh(d *db.DB) (bool, error) {
	if f.staticsTTL <= 0 {
		return false, nil
	}
	last, err := d.GetMeta(metaStaticsSync)
	if err != nil || last == "" {
		return false, err
	}
	t, terr := time.Parse(time.RFC3339, last)
	return terr == nil && time.Since(t) < f.staticsTTL, nil
}

// Fetch pulls issues (with all comments), cross-referenced PRs, and changelogs
// into the database. The first run is a full walk, committed page-by-page with
// its cursor so it resumes if interrupted; later runs sync incrementally via
// search on updated-since. --full forces a re-walk.
func (f *FlagData) Fetch(full bool) error {
	owner, name, err := f.RepoOwnerName()
	if err != nil {
		return err
	}

	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	client := f.NewGraphQL()
	started := db.Now()

	cursor, err := d.GetMeta(metaFetchCursor)
	if err != nil {
		return err
	}
	lastSync, err := d.GetMeta(MetaLastSync)
	if err != nil {
		return err
	}

	switch {
	case full || lastSync == "" || cursor != "":
		if cursor != "" {
			cout.Printf("<yellow>resuming</> interrupted fetch of <white>%s</>/<cyan>%s</>...\n", owner, name)
		} else {
			cout.Printf("fetching all open issues for <white>%s</>/<cyan>%s</>...\n", owner, name)
			cursor = ""
		}
		if err := f.fullWalk(d, client, owner, name, cursor); err != nil {
			return err
		}
	default:
		since, terr := time.Parse(time.RFC3339, lastSync)
		if terr != nil {
			return fmt.Errorf("unparseable last_sync %q — run with --full: %w", lastSync, terr)
		}
		// small overlap so nothing on the boundary is missed
		since = since.Add(-2 * time.Minute)
		cout.Printf("syncing issues in <white>%s</>/<cyan>%s</> updated since <yellow>%s</>...\n", owner, name, since.Format("2006-01-02 15:04"))
		if err := f.incremental(d, client, owner, name, since); err != nil {
			return err
		}
	}

	if err := d.SetMeta(MetaLastSync, started.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := d.DeleteMeta(metaFetchCursor); err != nil {
		return err
	}

	// neither path above can be trusted with state flips: the full walk only
	// sees OPEN issues, and the incremental rides the search API, whose index
	// lags — a close landing in that lag window would be missed forever once
	// the cursor moves past it. Reconciling against github's real open set
	// catches those, but costs ~35 requests, so it's opt-in: the report turns
	// it on by default (its accuracy IS the product), while apply paths are
	// already guarded by a live-state check before every close.
	if f.AutoReconcile {
		if err := f.reconcileOpenSet(d, client, owner, name); err != nil {
			return err
		}
	}

	if err := f.fetchRemainingComments(d, client, owner, name); err != nil {
		return err
	}

	if fresh, ferr := f.staticsFresh(d); ferr != nil {
		return ferr
	} else if fresh {
		cout.Printf("<gray>changelogs, docs and milestones refreshed within %s — skipped (koi fetch refreshes them)</>\n", staticsEvery)
	} else {
		if err := f.fetchChangelogs(d, client, owner, name); err != nil {
			return err
		}

		if err := f.fetchRemovals(d, client, owner, name); err != nil {
			return err
		}

		if err := f.fetchProviderDocs(d, client, owner, name); err != nil {
			return err
		}

		// front-load the milestone scan too: fetch does everything non-AI so every
		// other command can run offline afterwards (incremental, so cheap when fresh)
		if err := f.MilestoneScan(d, false); err != nil {
			return err
		}

		if err := d.SetMeta(metaStaticsSync, db.Now().Format(time.RFC3339)); err != nil {
			return err
		}
	}

	total, open, err := d.CountIssues()
	if err != nil {
		return err
	}
	cout.Printf("<green>done:</> <yellow>%d</> issues in db (<yellow>%d</> open)\n", total, open)

	// run the rules straight away so fetch is the only setup step — everything
	// downstream (review, report, stats) also re-runs this itself
	if err := f.EnsureAnalysed(d); err != nil {
		return err
	}
	cout.Printf("next: <cyan>koi close report</> for every close candidate with its evidence, or act on one check: <cyan>koi close fixed</> · <cyan>koi close resolved</> · <cyan>koi close comments</> · <cyan>koi close exists</> · <cyan>koi close legacy</> · <cyan>koi close deprecated</>\n")
	return nil
}

func (f *FlagData) fullWalk(d *db.DB, client *gh.Client, owner, name, cursor string) error {
	fetched := 0
	for {
		page, err := client.OpenIssues(owner, name, cursor)
		if err != nil {
			return err
		}

		bundles := make([]db.IssueBundle, 0, len(page.Issues))
		for i := range page.Issues {
			bundles = append(bundles, nodeToBundle(&page.Issues[i]))
		}

		// the page and its cursor commit together: an interrupted fetch resumes here
		if err := d.SaveIssues(bundles, metaFetchCursor, page.PageInfo.EndCursor); err != nil {
			return err
		}

		for i := range bundles {
			printFetchedIssue(fetched+i+1, page.TotalCount, &bundles[i])
		}
		fetched += len(page.Issues)
		cout.Printf("  <gray>%d/%d fetched · rate limit: %d remaining</>\n", fetched, page.TotalCount, page.RateLimit.Remaining)
		page.RateLimit.WaitIfLow()

		if !page.PageInfo.HasNextPage {
			return nil
		}
		cursor = page.PageInfo.EndCursor
	}
}

// printFetchedIssue is one line per fetched issue: position, number, state
// (green closed / orange open), title, and the facts that ride along.
func printFetchedIssue(pos, total int, b *db.IssueBundle) {
	i := &b.Issue
	state := text.StateTag(i.State)
	extra := fmt.Sprintf(" <gray>· 💬 %d</>", i.CommentCount)
	if i.ThumbsUp > 0 {
		extra += fmt.Sprintf(" <gray>· 👍 %d</>", i.ThumbsUp)
	}
	if major, _ := issue.VersionFromLabels(i.Labels); major > 0 {
		extra += fmt.Sprintf(" <gray>·</> <lightMagenta>v%d.x</>", major)
	}
	if prs := len(b.Crossrefs); prs > 0 {
		extra += fmt.Sprintf(" <gray>· %d crossref(s)</>", prs)
	}
	cout.Printf("  <gray>%6d/%d</> <cyan>#%-6d</> %s %s%s\n",
		pos, total, i.Number, state, text.TruncateRunes(text.OneLine(i.Title), 65), extra)
}

func (f *FlagData) incremental(d *db.DB, client *gh.Client, owner, name string, since time.Time) error {
	cursor, fetched := "", 0
	for {
		page, err := client.UpdatedIssues(owner, name, since, cursor)
		if err != nil {
			return err
		}

		// the search API silently caps at 1000 results; fall back to a full walk
		if page.IssueCount > 950 {
			cout.Printf("<yellow>%d issues changed since last sync — falling back to a full walk</>\n", page.IssueCount)
			return f.fullWalk(d, client, owner, name, "")
		}

		bundles := make([]db.IssueBundle, 0, len(page.Issues))
		for i := range page.Issues {
			bundles = append(bundles, nodeToBundle(&page.Issues[i]))
		}
		if err := d.SaveIssues(bundles, "", ""); err != nil {
			return err
		}

		for i := range bundles {
			printFetchedIssue(fetched+i+1, page.IssueCount, &bundles[i])
		}
		fetched += len(page.Issues)
		page.RateLimit.WaitIfLow()

		if !page.PageInfo.HasNextPage {
			if fetched == 0 {
				cout.Printf("  <gray>nothing changed</>\n")
			}
			return nil
		}
		cursor = page.PageInfo.EndCursor
	}
}

// reconcileOpenSet diffs the db's open set against the repository's actual
// open issue numbers (the repository connection is ground truth; search is
// not) and refetches every issue whose state disagrees: closes the sync
// missed, reopens, and brand-new issues alike.
func (f *FlagData) reconcileOpenSet(d *db.DB, client *gh.Client, owner, name string) error {
	if last, err := d.GetMeta(metaLastReconcile); err != nil {
		return err
	} else if at, terr := time.Parse(time.RFC3339, last); terr == nil && time.Since(at) < reconcileEvery {
		cout.Printf("<gray>open set verified against github %dm ago — skipping</>\n", int(time.Since(at).Minutes()))
		return nil
	}

	cout.Printf("verifying the local open set against github (numbers only, the sync's search index can lag)...\n")
	live, err := client.OpenIssueNumbers(owner, name, func(fetched, total int) {
		cout.Printf("  <gray>%d/%d open on github</>\n", fetched, total)
	})
	if err != nil {
		return err
	}
	states, err := d.IssueStates()
	if err != nil {
		return err
	}

	var stale []int
	for number, state := range states {
		if state == db.IssueOpen && !live[number] {
			stale = append(stale, number) // closed (or deleted) on github, open here
		}
	}
	for number := range live {
		if states[number] != db.IssueOpen {
			stale = append(stale, number) // reopened or never fetched, closed/absent here
		}
	}
	if len(stale) == 0 {
		cout.Printf("  <gray>open set matches github (%d open)</>\n", len(live))
		return d.SetMeta(metaLastReconcile, db.Now().Format(time.RFC3339))
	}

	slices.Sort(stale)
	cout.Printf("<yellow>%d</> issue(s) out of sync with github (the search index lags) — refetching...\n", len(stale))
	nodes, err := client.IssuesByNumber(owner, name, stale)
	if err != nil {
		return err
	}
	bundles := make([]db.IssueBundle, 0, len(nodes))
	for i := range nodes {
		bundles = append(bundles, nodeToBundle(&nodes[i]))
	}
	if err := d.SaveIssues(bundles, "", ""); err != nil {
		return err
	}
	for i := range bundles {
		printFetchedIssue(i+1, len(bundles), &bundles[i])
	}
	return d.SetMeta(metaLastReconcile, db.Now().Format(time.RFC3339))
}

// fetchRemainingComments pages in the tail comments of issues whose comment count
// exceeds what the bulk fetch returned — old, busy issues, exactly the ones where
// the comments carry the decisive context.
func (f *FlagData) fetchRemainingComments(d *db.DB, client *gh.Client, owner, name string) error {
	// closed issues koi acted on stay included: the incremental sync refetches
	// them on new activity but only carries the FIRST 50 comments, and on a
	// long thread the post-close comments — what koi close review reads — live
	// past that page
	rows, err := d.Query(`
		SELECT i.number, i.comment_count, COUNT(c.id)
		FROM issues i LEFT JOIN comments c ON c.issue_number = i.number
		WHERE i.state = 'OPEN'
		   OR i.number IN (SELECT issue_number FROM actions WHERE action = 'close' AND status = 'applied')
		GROUP BY i.number HAVING i.comment_count > COUNT(c.id)`)
	if err != nil {
		return fmt.Errorf("finding issues with missing comments: %w", err)
	}

	type missing struct{ number, want, have int }
	var todo []missing
	for rows.Next() {
		var m missing
		if err := rows.Scan(&m.number, &m.want, &m.have); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning missing-comments row: %w", err)
		}
		todo = append(todo, m)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating missing-comments rows: %w", err)
	}

	if len(todo) == 0 {
		return nil
	}
	cout.Printf("fetching remaining comments for <yellow>%d</> long-thread issues...\n", len(todo))

	for n, m := range todo {
		cursor := ""
		for {
			nodes, pageInfo, err := client.MoreComments(owner, name, m.number, cursor)
			if err != nil {
				// non-fatal: the first 50 comments are already in the db
				cout.Errorf("  <red>#%d:</> %v\n", m.number, err)
				break
			}
			comments := make([]db.Comment, 0, len(nodes))
			for i := range nodes {
				comments = append(comments, commentFromNode(&nodes[i], m.number))
			}
			if err := d.AppendComments(comments); err != nil {
				return err
			}
			if !pageInfo.HasNextPage {
				break
			}
			cursor = pageInfo.EndCursor
		}
		if (n+1)%25 == 0 {
			cout.Printf("  <yellow>%d</>/<yellow>%d</>\n", n+1, len(todo))
		}
	}

	return nil
}

// rawFileRetry downloads a raw file with a few retries — raw.githubusercontent
// throttles bursts, and a transient failure here must not look like a 404. A
// real 404 is permanent, so it fails immediately instead of burning retries.
func rawFileRetry(client *gh.Client, owner, name, file string) (string, error) {
	var content string
	var err error
	for attempt := 1; ; attempt++ {
		content, err = client.RawFile(owner, name, "main", file)
		if err == nil {
			return content, nil
		}
		if errors.Is(err, gh.ErrNotFound) || attempt == 3 {
			return "", err
		}
		clog.Log.Debugf("changelog %s attempt %d: %v", file, attempt, err)
		time.Sleep(3 * time.Second)
	}
}

// changelogFiles are the per-major changelog files in the azurerm repo: the
// live CHANGELOG.md plus an archived CHANGELOG-vN.md per shipped major below
// the current one — derived so a major bump fetches the newly archived file.
func changelogFiles(currentMajor int) []string {
	files := []string{"CHANGELOG.md"}
	for major := currentMajor - 1; major >= 0; major-- {
		files = append(files, fmt.Sprintf("CHANGELOG-v%d.md", major))
	}
	return files
}

func (f *FlagData) fetchChangelogs(d *db.DB, client *gh.Client, owner, name string) error {
	var entries []db.ChangelogEntry
	failed := 0
	for _, file := range changelogFiles(f.CurrentMajor) {
		content, err := rawFileRetry(client, owner, name, file)
		if err != nil {
			// a 404 is normal (repos without split changelogs have fewer files) but
			// anything transient must be loud: a silent skip here once replaced the
			// changelog table with half its entries
			failed++
			cout.Errorf("<yellow>warning:</> changelog %s: %v\n", file, err)
			continue
		}
		entries = append(entries, issue.ParseChangelog(content)...)
	}

	if len(entries) == 0 {
		cout.Errorf("<yellow>warning:</> no changelog entries parsed\n")
		return nil
	}
	if failed > 0 {
		if existing, err := d.CountChangelog(); err == nil && len(entries) < existing {
			cout.Errorf("<yellow>warning:</> only %d changelog entries parsed (%d files failed) but the db already has %d — keeping the existing table\n",
				len(entries), failed, existing)
			return nil
		}
	}

	if err := d.ReplaceChangelog(entries); err != nil {
		return err
	}
	cout.Printf("parsed <yellow>%d</> changelog entries\n", len(entries))
	return nil
}

// removalGuideMajors are the majors whose upgrade guides use the parseable
// removed-sections format: 4.0 onwards through the current major (the 3.0
// guide predates it and carries little) — derived so a major bump fetches the
// new guide.
func removalGuideMajors(currentMajor int) []int {
	var majors []int
	for major := 4; major <= currentMajor; major++ {
		majors = append(majors, major)
	}
	return majors
}

// fetchRemovals rebuilds the removed/deprecated inventory from the upgrade
// guides plus the changelog's DEPRECATIONS bullets. A failed guide download
// keeps the existing table — a transient failure must not empty it.
func (f *FlagData) fetchRemovals(d *db.DB, client *gh.Client, owner, name string) error {
	var removals []db.Removal
	for _, major := range removalGuideMajors(f.CurrentMajor) {
		file := fmt.Sprintf("website/docs/guides/%d.0-upgrade-guide.html.markdown", major)
		content, err := rawFileRetry(client, owner, name, file)
		if err != nil {
			cout.Errorf("<yellow>warning:</> upgrade guide %s: %v — keeping the existing removals table\n", file, err)
			return nil
		}
		removals = append(removals, issue.ParseUpgradeGuide(content, major)...)
	}

	deprecations, err := d.ChangelogSection("DEPRECATIONS")
	if err != nil {
		return err
	}
	removals = append(removals, issue.MineChangelogDeprecations(deprecations)...)

	if err := d.ReplaceRemovals(removals); err != nil {
		return err
	}
	removed, deprecated := 0, 0
	for _, r := range removals {
		if r.Action == db.RemovalRemoved {
			removed++
		} else {
			deprecated++
		}
	}
	cout.Printf("parsed <yellow>%d</> removals from the upgrade guides + changelog (<red>%d removed</> · <yellow>%d deprecated</>)\n",
		len(removals), removed, deprecated)
	return nil
}

// docArgBullet spots "* `arg` -" documentation bullets naming an argument or
// attribute.
var docArgBullet = regexp.MustCompile("(?m)^\\s*[*-]\\s+`([a-z0-9_.]+)`")

// fetchProviderDocs rebuilds the what-exists-now inventory from the website
// docs: the resource/data source listings, and every argument each doc page
// names TODAY. Page contents are only refetched when the docs trees actually
// changed (their oids are remembered). A failed listing keeps existing data.
func (f *FlagData) fetchProviderDocs(d *db.DB, client *gh.Client, owner, name string) error {
	kinds := map[string]string{db.DocKindResource: "website/docs/r", db.DocKindDataSource: "website/docs/d"}
	byKind := map[string][]string{}
	paths := map[string][]string{}
	var oids []string
	for _, kind := range []string{db.DocKindResource, db.DocKindDataSource} {
		path := kinds[kind]
		entries, oid, err := client.TreeEntries(owner, name, path)
		if err != nil || len(entries) == 0 {
			cout.Errorf("<yellow>warning:</> listing %s: %v — keeping the existing provider docs inventory\n", path, err)
			return nil
		}
		oids = append(oids, oid)
		for _, e := range entries {
			if n, ok := strings.CutSuffix(e, ".html.markdown"); ok {
				byKind[kind] = append(byKind[kind], "azurerm_"+n)
				paths[kind] = append(paths[kind], path+"/"+e)
			}
		}
	}
	if err := d.ReplaceProviderDocs(byKind); err != nil {
		return err
	}
	cout.Printf("listed <yellow>%d</> resources + <yellow>%d</> data sources that exist in the provider\n",
		len(byKind[db.DocKindResource]), len(byKind[db.DocKindDataSource]))

	sha := strings.Join(oids, "|")
	// the argument inventory needs every doc page's contents — only re-read
	// them when the docs trees changed since last time
	if last, err := d.GetMeta("docs_args_sha"); err != nil {
		return err
	} else if last == sha {
		cout.Printf("<gray>provider doc pages unchanged — keeping the documented-argument inventory</>\n")
		return nil
	}

	var args []db.DocArg
	for _, kind := range []string{db.DocKindResource, db.DocKindDataSource} {
		cout.Printf("reading <yellow>%d</> %s doc pages for their documented arguments...\n", len(paths[kind]), strings.ReplaceAll(kind, "-", " "))
		texts, err := client.BlobTexts(owner, name, paths[kind], func(done, total int) {
			cout.Printf("  <gray>%d/%d pages read</>\n", done, total)
		})
		if err != nil {
			cout.Errorf("<yellow>warning:</> reading doc pages: %v — keeping the existing argument inventory\n", err)
			return nil
		}
		for p, content := range texts {
			base := strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".html.markdown")
			seen := map[string]bool{}
			for _, m := range docArgBullet.FindAllStringSubmatch(content, -1) {
				if seen[m[1]] {
					continue
				}
				seen[m[1]] = true
				args = append(args, db.DocArg{Kind: kind, Name: "azurerm_" + base, Arg: m[1]})
			}
		}
	}
	if err := d.ReplaceDocArgs(args); err != nil {
		return err
	}
	cout.Printf("parsed <yellow>%d</> documented arguments across the provider docs\n", len(args))
	return d.SetMeta("docs_args_sha", sha)
}

// nodeToBundle converts a GraphQL issue node into db rows.
func nodeToBundle(n *gh.IssueNode) db.IssueBundle {
	labels := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labels = append(labels, l.Name)
	}

	b := db.IssueBundle{
		Issue: db.Issue{
			Number:            n.Number,
			Title:             n.Title,
			Body:              n.Body,
			State:             n.State,
			StateReason:       n.StateReason,
			Author:            n.Author.Login,
			AuthorAssociation: n.AuthorAssociation,
			CreatedAt:         n.CreatedAt,
			UpdatedAt:         n.UpdatedAt,
			ClosedAt:          n.ClosedAt,
			Labels:            labels,
			CommentCount:      n.Comments.TotalCount,
			ThumbsUp:          n.Thumbs.TotalCount,
			ReactionsTotal:    n.Reactions.TotalCount,
			URL:               n.URL,
			FetchedAt:         db.Now(),
		},
	}

	for i := range n.Comments.Nodes {
		b.Comments = append(b.Comments, commentFromNode(&n.Comments.Nodes[i], n.Number))
	}

	for _, t := range n.TimelineItems.Nodes {
		s := t.Source
		if s.Number == 0 {
			continue // inaccessible or deleted source
		}
		ref := db.Crossref{
			WillClose:   t.WillCloseTarget,
			IssueNumber: n.Number,
			RefRepo:     s.Repository.NameWithOwner,
			RefNumber:   s.Number,
			IsPR:        s.Typename == TypePullRequest,
			Merged:      s.Merged,
			MergedAt:    s.MergedAt,
			Title:       s.Title,
		}
		if ref.IsPR {
			ref.State = s.PRState
		} else {
			ref.State = s.IssueState
		}
		b.Crossrefs = append(b.Crossrefs, ref)
	}

	return b
}

func commentFromNode(n *gh.CommentNode, issueNumber int) db.Comment {
	return db.Comment{
		ID:                n.ID,
		IssueNumber:       issueNumber,
		Author:            n.Author.Login,
		AuthorAssociation: n.AuthorAssociation,
		CreatedAt:         n.CreatedAt,
		Body:              n.Body,
		URL:               n.URL,
	}
}

// EnsureAnalysed refreshes signals and rule proposals before a consumer command
// runs. Analyse is deterministic, takes seconds, and never overwrites human
// decisions or AI enrichment — so commands re-run it themselves rather than
// making the user remember to.
func (f *FlagData) EnsureAnalysed(d *db.DB) error {
	_, err := issue.AnalyseAll(d, f.GH.Repo, f.RuleConfig())
	return err
}
