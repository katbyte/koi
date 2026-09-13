package close

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
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

const (
	passDocs          = "docs"
	promptDocs        = "issue-docs-close"
	templateDocsClose = "docs-close"
	reasonDocs        = "docs-updated"

	// the signal kind this check owns
	signalKindDocs = "documentation"

	// docsJudgeBatch is this pass's pairings per AI call: blocks ship whole doc
	// pages, so fewer per call buys each one more room (p90 page ≈ 8.6KB).
	docsJudgeBatch = 5
	// docsPageRunes is the size up to which a page ships to the judge WHOLE;
	// bigger pages (p99 ≈ 32KB) are condensed around the issue's terms instead,
	// with the same budget. docsMaxPages / docsMaxContent cap how many pages an
	// issue resolves to and how many of those ship content.
	docsPageRunes  = 16000
	docsMaxPages   = 3
	docsMaxContent = 2
	// docsMaxAskTokens caps the issue terms a page digest is built around.
	docsMaxAskTokens = 8
)

// docsPage is one documentation page an issue concerns, with its fate in the
// checkout: whether it still exists at the ref, and how it moved since the
// report.
type docsPage struct {
	kind     string // db.DocKindResource | db.DocKindDataSource
	name     string // azurerm_thing
	path     string // website/docs/r/thing.html.markdown
	exists   bool
	commits  int       // commits touching the page since the issue was opened
	lastEdit time.Time // the newest such commit
}

// docsFinding is one open documentation issue whose page(s) changed after the
// report — whether the change addressed the ask is the judge's call. best
// indexes the most-edited page; the close comment cites it.
type docsFinding struct {
	issue *db.Issue
	pages []docsPage
	best  int
}

// DocsOpts configures the docs audit and its apply modes.
type DocsOpts struct {
	Src                 string // local provider checkout to read
	Ref                 string // git ref treated as the current source
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// Docs finds OPEN documentation issues whose page has been revised since the
// report. Edits alone prove nothing — doc pages churn constantly — so the AI
// reads the CURRENT page content against the issue's specific ask before
// blessing a close; closes are completed, pointing at the updated page.
func (f *Flags) Docs() error {
	o := DocsOpts{Src: f.Cmd.Errors.ProviderSrc, Ref: f.Cmd.Errors.ProviderRef, FlagsApplyModes: f.Modes}
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

	col, err := f.collectDocs(d, o)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d of %d open documentation issues concern a page revised since the report:</>\n", len(findings), col.docs)
	cout.Printf("  <gray>skipped: %d whose pages are untouched since the report · %d whose pages no longer exist · %d naming no known doc page · %s</>\n",
		col.untouched, col.removed, col.unresolved, keepSummary(col.protected))
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyDocs(d, findings, o, true)
	case o.Apply:
		return f.applyDocs(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.docsJudgeItems(d, findings, o.Src, o.Ref)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocksBatch(d, passDocs, promptText, docsJudgeBatch, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b docsFinding) int {
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
		f.printDocsCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close docs --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// docsCollection is everything collectDocs learns in one scan.
type docsCollection struct {
	findings   []docsFinding
	open       int // open issues in the db
	docs       int // open documentation-kind issues
	untouched  int // pages unedited since the report — the complaint likely stands
	removed    int // every resolved page is gone from the ref (deprecated territory)
	unresolved int // no doc page could be resolved from the issue
	protected  map[string]int
}

// Doc-page pointers in issue text: registry links and repository paths.
var (
	reDocsRegistryURL = regexp.MustCompile(`registry\.terraform\.io/providers/[\w./-]*?/docs/(resources|data-sources)/([a-z0-9_]+)`)
	reDocsRepoPath    = regexp.MustCompile(`website/docs/([rd])/([a-z0-9_]+)`)
)

// docsPagePath is where a documented thing's page lives in the repository.
func docsPagePath(kind, name string) string {
	dir := "r"
	if kind == db.DocKindDataSource {
		dir = "d"
	}
	return "website/docs/" + dir + "/" + strings.TrimPrefix(name, "azurerm_") + ".html.markdown"
}

// resolveDocsPages maps one issue to the doc pages it concerns: explicit
// registry/repository links first (the surest pointer), then the issue's
// resources — as resource pages, and as data-source pages when the issue talks
// about a data source.
func resolveDocsPages(i *db.Issue, s *db.Signals, docs map[string]bool) []docsPage {
	var pages []docsPage
	seen := map[string]bool{}
	add := func(kind, name string) {
		name = "azurerm_" + strings.TrimPrefix(name, "azurerm_")
		key := kind + "|" + name
		if seen[key] || len(pages) >= docsMaxPages {
			return
		}
		seen[key] = true
		pages = append(pages, docsPage{kind: kind, name: name, path: docsPagePath(kind, name)})
	}
	for _, m := range reDocsRegistryURL.FindAllStringSubmatch(i.Body, -1) {
		kind := db.DocKindResource
		if m[1] == "data-sources" {
			kind = db.DocKindDataSource
		}
		add(kind, m[2])
	}
	for _, m := range reDocsRepoPath.FindAllStringSubmatch(i.Body, -1) {
		kind := db.DocKindResource
		if m[1] == "d" {
			kind = db.DocKindDataSource
		}
		add(kind, m[2])
	}
	mentionsDS := strings.Contains(strings.ToLower(i.Title+"\n"+i.Body), "data source")
	for _, res := range s.Resources {
		if docs[db.DocKindResource+"|"+res] {
			add(db.DocKindResource, res)
		}
		if mentionsDS && docs[db.DocKindDataSource+"|"+res] {
			add(db.DocKindDataSource, res)
		}
	}
	return pages
}

// collectDocs walks the open documentation-kind issues, resolves the pages
// each concerns, and asks the checkout what happened to them since the report:
// pages with no edits since mean the complaint likely stands (skipped), pages
// that are gone belong to the deprecated check, and pages revised since are
// the candidates the judge reads.
func (f *Flags) collectDocs(d *db.DB, o DocsOpts) (*docsCollection, error) {
	col := &docsCollection{protected: map[string]int{}}
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	col.open = len(issues)
	if col.open == 0 {
		return col, nil
	}

	if err := verifyProviderSrc(o.Src, o.Ref); err != nil {
		return nil, err
	}
	docs, err := d.ProviderDocs()
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		cout.Printf("<yellow>no provider docs inventory — run koi fetch first</>\n")
		return col, nil
	}

	cout.Printf("checking documentation issues' pages against <cyan>%s</> at <lightMagenta>%s</>...\n", o.Src, o.Ref)
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, serr
		}
		if s == nil || s.Kind != signalKindDocs {
			continue
		}
		col.docs++

		switch {
		case s.OpenLinkedPRs > 0:
			col.protected["open-pr"]++
			continue
		case i.ThumbsUp >= f.KeepReactions:
			col.protected["high-engagement"]++
			continue
		}

		pages := resolveDocsPages(i, s, docs)
		if len(pages) == 0 {
			col.unresolved++
			continue
		}
		changed, anyExists := false, false
		for pi := range pages {
			p := &pages[pi]
			p.exists = docsGitExists(o.Src, o.Ref, p.path)
			if !p.exists {
				continue
			}
			anyExists = true
			if p.commits, p.lastEdit, err = docsGitHistory(o.Src, o.Ref, p.path, i.CreatedAt); err != nil {
				return nil, err
			}
			changed = changed || p.commits > 0
		}
		switch {
		case !anyExists:
			col.removed++
		case !changed:
			col.untouched++
		default:
			fdg := docsFinding{issue: i, pages: pages}
			for pi, p := range pages {
				if p.commits > pages[fdg.best].commits {
					fdg.best = pi
				}
			}
			col.findings = append(col.findings, fdg)
		}
	}

	slices.SortStableFunc(col.findings, func(a, b docsFinding) int {
		return a.issue.Number - b.issue.Number
	})
	return col, nil
}

// applyDocs is both apply modes on the shared harness: plain --apply closes
// everything listed (edits since the report prove nothing by themselves, so it
// exists for pattern consistency); --apply-with-ai[-auto] gates each close on
// the judge reading the current page and is the recommended path.
func (f *Flags) applyDocs(d *db.DB, findings []docsFinding, o DocsOpts, withAI bool) error {
	byNumber := map[int]*docsFinding{}
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
			return f.closeOneDocs(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "documentation issues whose page moved on"
	p.GateLabel = "addressed"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as completed in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.docsJudgeItems(d, findings, o.Src, o.Ref)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocksBatch(d, passDocs, promptText, docsJudgeBatch, items, onReady, onBatch)
		return jerr
	})
}

// closeOneDocs handles one candidate: card, the docs-close comment citing the
// most-edited page, and the close as completed (or preview under dry-run, or
// the a/s ask when interactive).
func (f *Flags) closeOneDocs(d *db.DB, repo gh.Repo, fdg *docsFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printDocsCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := f.renderDocsComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateDocsClose, issue.StateCompleted)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as addressed in the docs?", fdg.issue.Number), comment, fdg.issue.URL)
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
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reasonDocs)

	best := fdg.pages[fdg.best]
	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reasonDocs,
		StateReason: issue.StateCompleted, Template: templateDocsClose,
		Evidence: map[string]string{
			"page": best.name, "edits": strconv.Itoa(best.commits),
			"last-edit": best.lastEdit.Format("2006-01-02"), "url": registryDocURL(best.kind, best.name),
		},
		Source:         passDocs,
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

// renderDocsComment renders the close comment citing the most-edited page.
func (f *Flags) renderDocsComment(fdg *docsFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateDocsClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateDocsClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateDocsClose, err)
	}
	best := fdg.pages[fdg.best]
	data := struct {
		Name  string
		Times int
		Date  string
		URL   string
	}{best.name, best.commits, best.lastEdit.Format("January 2006"), registryDocURL(best.kind, best.name)}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateDocsClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// docsJudgeItems renders one judge block per finding: the issue's ask, the
// thread digest, and the CURRENT content of the pages it concerns — reading
// the page against the ask is the whole check.
func (f *Flags) docsJudgeItems(d *db.DB, findings []docsFinding, src, ref string) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptDocs)
	if err != nil {
		return "", nil, err
	}

	contents := map[string]string{}
	items := make([]issue.JudgeItem, 0, len(findings))
	for i := range findings {
		fdg := &findings[i]
		tokens := docsAskTokens(fdg.issue)
		comments, cerr := d.CommentsFor(fdg.issue.Number)
		if cerr != nil {
			return "", nil, cerr
		}

		var b strings.Builder
		fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.issue.Number, text.OneLine(fdg.issue.Title))
		fmt.Fprintf(&b, "opened %s, last activity %s\n", fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "ISSUE COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		b.WriteString("DOC PAGES THE ISSUE CONCERNS:\n")
		shown := 0
		for _, p := range fdg.pages {
			switch {
			case !p.exists:
				fmt.Fprintf(&b, "- `%s` (%s): PAGE NO LONGER EXISTS at %s\n", p.name, p.kind, ref)
				continue
			case p.commits == 0:
				fmt.Fprintf(&b, "- `%s` (%s): NOT edited since the report\n", p.name, p.kind)
				continue
			}
			fmt.Fprintf(&b, "- `%s` (%s): edited %d times since the report, most recently %s\n",
				p.name, p.kind, p.commits, p.lastEdit.Format("2006-01-02"))
			if shown == docsMaxContent {
				continue
			}
			shown++
			c, ok := contents[p.path]
			if !ok {
				c = docsGitShow(src, ref, p.path)
				contents[p.path] = c
			}
			switch {
			case c == "":
			case len([]rune(c)) > docsPageRunes:
				fmt.Fprintf(&b, "CURRENT PAGE CONTENT for `%s` (large page, condensed around the issue's terms):\n%s\n", p.name, docsPageDigest(c, tokens))
			default:
				fmt.Fprintf(&b, "CURRENT PAGE CONTENT for `%s` (complete):\n%s\n%s", p.name, c, docsMissingTerms(c, tokens))
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printDocsCard is one candidate: the issue and each page's movement since the
// report, with the AI's score when judged.
func (f *Flags) printDocsCard(fdg *docsFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	for _, p := range fdg.pages {
		switch {
		case !p.exists:
			cout.Printf("      <lightCyan>%s</> <gray>(%s) —</> <red>page no longer exists</>\n", p.name, p.kind)
		case p.commits == 0:
			cout.Printf("      <lightCyan>%s</> <gray>(%s) — untouched since the report</>\n", p.name, p.kind)
		default:
			cout.Printf("      <lightCyan>%s</> <gray>(%s) —</> <green>edited %d time%s since the report</><gray>, last %s ·</> <darkGray>%s</>\n",
				p.name, p.kind, p.commits, map[bool]string{true: "", false: "s"}[p.commits == 1],
				p.lastEdit.Format("2006-01-02"), registryDocURL(p.kind, p.name))
		}
	}
	cli.PrintVerdict(v)
}

// docsAskTokens pulls the terms an issue's ask is about — backticked tokens
// and snake_case words from the title and prose (config dumps are not intent)
// — so the page digest can be built around them. azurerm_* names are the
// page, not the ask.
func docsAskTokens(i *db.Issue) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reDocsAskToken.FindAllStringSubmatch(i.Title+"\n"+issue.Prose(i.Body), -1) {
		tok := strings.ToLower(m[1] + m[2]) // one group matches, the other is empty
		if seen[tok] || strings.HasPrefix(tok, "azurerm_") || len(out) >= docsMaxAskTokens {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

var reDocsAskToken = regexp.MustCompile("`([A-Za-z0-9_.]{3,40})`" + `|\b([a-z0-9]+(?:_[a-z0-9]+)+)\b`)

// docsPageDigest condenses a doc page for a judge block. A plain prefix cut
// hid exactly the entries the judge needed (a page's relevant argument can sit
// anywhere in 50KB), so instead: every line mentioning an issue term survives
// with two lines of context, then headings, argument/attribute entries, and
// the intro fill the remaining budget in page order; terms absent from the
// whole page are named outright — absence is decisive evidence too.
func docsPageDigest(content string, tokens []string) string {
	lines := strings.Split(content, "\n")
	hasToken := func(s string) bool {
		low := strings.ToLower(s)
		return slices.ContainsFunc(tokens, func(t string) bool { return strings.Contains(low, t) })
	}
	isEntry := func(s string) bool {
		t := strings.TrimSpace(s)
		return strings.HasPrefix(t, "* `") || strings.HasPrefix(t, "- `")
	}

	keep := make([]bool, len(lines))
	budget := docsPageRunes
	take := func(idx int) {
		if idx < 0 || idx >= len(lines) || keep[idx] || budget <= 0 {
			return
		}
		keep[idx] = true
		budget -= len([]rune(lines[idx])) + 1
	}
	// tier 1: issue-term hits with context — these must never fall to the cap
	for idx, ln := range lines {
		if hasToken(ln) {
			take(idx - 1)
			take(idx)
			take(idx + 1)
		}
	}
	// tier 2: structure (headings, entries) and the intro, while budget lasts
	for idx, ln := range lines {
		if budget <= 0 {
			break
		}
		if idx < 10 || isEntry(ln) || strings.HasPrefix(strings.TrimSpace(ln), "#") {
			take(idx)
		}
	}

	var b strings.Builder
	gap := false
	for idx, ln := range lines {
		if !keep[idx] {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteString("…\n")
		}
		gap = false
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	b.WriteString(docsMissingTerms(content, tokens))
	return b.String()
}

// docsMissingTerms names the issue terms absent from the entire page ("" when
// none) — for a "please document X" ask, absence is decisive evidence.
func docsMissingTerms(content string, tokens []string) string {
	var missing []string
	low := strings.ToLower(content)
	for _, t := range tokens {
		if !strings.Contains(low, t) {
			missing = append(missing, "`"+t+"`")
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "ISSUE TERMS THAT APPEAR NOWHERE IN THIS PAGE: " + strings.Join(missing, ", ") + "\n"
}

// Git plumbing for reading doc pages out of the checkout at the ref.

// docsGitExists reports whether the path exists in the tree at the ref.
func docsGitExists(src, ref, path string) bool {
	cmd := exec.CommandContext(context.Background(), "git", "-C", src, "cat-file", "-e", ref+":"+path) //nolint:gosec // G204: the checkout and ref are user-configured on purpose (--provider-src/-ref)
	return cmd.Run() == nil
}

// docsGitHistory returns how many commits touched the path at the ref since
// the given time, and the newest one's date.
func docsGitHistory(src, ref, path string, since time.Time) (int, time.Time, error) {
	cmd := exec.CommandContext(context.Background(), "git", "-C", src, "log", "--format=%cI", //nolint:gosec // G204: the checkout and ref are user-configured on purpose (--provider-src/-ref)
		"--since="+since.Format(time.RFC3339), ref, "--", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("git log for %s in %s: %w", path, src, err)
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return 0, time.Time{}, nil
	}
	last, perr := time.Parse(time.RFC3339, lines[0])
	if perr != nil {
		return len(lines), time.Time{}, nil //nolint:nilerr // the count still stands; the date is cosmetic
	}
	return len(lines), last, nil
}

// docsGitShow returns the file's content at the ref ("" when unreadable — the
// ref is verified up front, so that means the path is gone).
func docsGitShow(src, ref, path string) string {
	cmd := exec.CommandContext(context.Background(), "git", "-C", src, "show", ref+":"+path) //nolint:gosec // G204: the checkout and ref are user-configured on purpose (--provider-src/-ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
