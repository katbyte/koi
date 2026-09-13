// The close-candidates report: one section per check, riding the shared
// report scaffolding in package cli.

package close

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/cli"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// prHTMLURL and issHTMLURL build web urls for report links.
func (f *Flags) prHTMLURL(n int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", f.GH.Repo, n)
}

func (f *Flags) issHTMLURL(n int) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", f.GH.Repo, n)
}

// Report writes close-<stamp>.html: every close candidate each check sees (fixed,
// resolved, duplicates, comments, exists, legacy, errors, deprecated), with
// the evidence for why it is listed and links to
// everything cited. --with-ai scores each candidate with the check's judge
// (cached verdicts are reused) and sorts surest first; --limit N keeps test
// runs cheap. The old analyse-based report and its decisions.csv are gone —
// the checks' apply modes are the review flow now.
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
	// the errors and docs checks read a provider checkout; a report missing
	// two checks would be acted on as if it were complete, so fail up front —
	// before the auto-fetch and the expensive scans — rather than silently
	// under-report
	if err := verifyProviderSrc(f.Cmd.Errors.ProviderSrc, f.Cmd.Errors.ProviderRef); err != nil {
		return fmt.Errorf("the errors and docs checks need a provider checkout: %w", err)
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

	now := time.Now()
	data := cli.ReportData{Repo: f.GH.Repo, Noun: "close candidates", WithAI: o.WithAI, GeneratedAt: now.Format("2006-01-02 15:04")}

	fixed, err := f.fixedReportSection(d, o, now)
	if err != nil {
		return err
	}
	resolved, err := f.resolvedReportSection(d, o, now)
	if err != nil {
		return err
	}
	legacy, err := f.legacyReportSection(d, o, now)
	if err != nil {
		return err
	}
	deprecated, err := f.deprecatedReportSection(d, o, now)
	if err != nil {
		return err
	}
	comments, err := f.commentsReportSection(d, o, now)
	if err != nil {
		return err
	}
	exists, err := f.existsReportSection(d, o, now)
	if err != nil {
		return err
	}
	errorsSec, err := f.errorsReportSection(d, o, now)
	if err != nil {
		return err
	}
	questions, err := f.questionsReportSection(d, o, now)
	if err != nil {
		return err
	}
	docsSec, err := f.docsReportSection(d, o, now)
	if err != nil {
		return err
	}
	stale, err := f.staleReportSection(d, o, now)
	if err != nil {
		return err
	}
	data.Sections = []cli.ReportSection{fixed, resolved, comments, questions, stale, exists, legacy, errorsSec, docsSec, deprecated}
	for _, s := range data.Sections {
		data.Total += s.Total
	}
	if data.Total == 0 {
		cout.Printf("no close candidates in any check — is the db fetched? (<cyan>koi fetch</>)\n")
		return nil
	}

	if err := os.MkdirAll(o.Out, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", o.Out, err)
	}
	htmlPath := filepath.Join(o.Out, cli.ReportFileName("close", now))
	if err := cli.WriteReportHTML(htmlPath, &data); err != nil {
		return err
	}
	cout.Printf("\nwrote <cyan>%s</> — <yellow>%d</> close candidates <gray>(fixed %d · resolved %d · comments %d · questions %d · stale %d · exists %d · legacy %d · errors %d · docs %d · deprecated %d)</>\n",
		htmlPath, data.Total, fixed.Total, resolved.Total, comments.Total, questions.Total, stale.Total, exists.Total, legacy.Total, errorsSec.Total, docsSec.Total, deprecated.Total)

	// the companions: the ledger of everything closed, and the review of it
	closedHTML, closedCSV, actData, err := f.writeActionsTaken(d, o.Out, now)
	if err != nil {
		return err
	}
	if closedHTML != "" {
		cout.Printf("wrote <cyan>%s</> and <cyan>%s</> — <yellow>%d</> actions taken\n", closedHTML, closedCSV, actData.Total)
	}
	reviewHTML, reviewData, err := f.writeReviewReport(d, o, now)
	if err != nil {
		return err
	}
	if reviewHTML != "" {
		cout.Printf("wrote <cyan>%s</> — <yellow>%d</> disputed · <yellow>%d</> reopened\n",
			reviewHTML, reviewData.Sections[0].Total, reviewData.Sections[1].Total)
	}
	if !o.WithAI {
		cout.Printf("<gray>rerun with</> <cyan>--with-ai</> <gray>to score every candidate, or</> <cyan>--limit 10</> <gray>to test cheaply</>\n")
	}
	// print file:// urls so the terminal makes every page clickable
	for _, p := range []string{htmlPath, closedHTML, reviewHTML} {
		if p == "" {
			continue
		}
		if abs, aerr := filepath.Abs(p); aerr == nil {
			cout.Printf("<gray>open:</> <cyan>file://%s</>\n", abs)
		}
	}
	return nil
}

// fixedReportSection builds the "a merged PR touches this" check section.
func (f *Flags) fixedReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passFixed,
		Name:     "close " + passFixed,
		Question: "a shipped fix appears to cover this open issue — did it?",
		Description: "Every open issue a shipped code change appears to fix: a merged same-repository PR references it " +
			"(fixed-by = closing keyword, mentioned-by = bare mention), or — for bug reports — an uncited BUG FIXES " +
			"changelog bullet on its resource postdates both the report and the version it reported against (matched = " +
			"the fix description shares the report's own property or symptom, resource-only = leads). All the fix " +
			"evidence for an issue is judged together. Applying closes as completed citing the PR and shipped release, " +
			"or the bullet and its release.",
		Command: "koi close fixed [fixed-by|mentioned-by|matched|resource-only] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	col, err := f.collectFixed(d, "")
	if err != nil {
		return s, err
	}
	findings, prVersions := col.findings, col.prVersions
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classFixedBy, Count: col.counts[classFixedBy], Kind: cli.KindOK},
		{Name: classMentionedBy, Count: col.counts[classMentionedBy], Kind: cli.KindWarn},
		{Name: classClMatched, Count: col.counts[classClMatched], Kind: cli.KindMid},
		{Name: classClResourceOnly, Count: col.counts[classClResourceOnly], Kind: cli.KindDim},
	}
	s.Note = fmt.Sprintf("bullet sweep: %d open bug reports scanned · %d skipped with every candidate fix predating the reported version", col.bugs, col.predated)

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.fixedJudgeItems(d, findings, prVersions)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passFixed, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *fixedFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for n := range fdg.prs {
			pr := &fdg.prs[n]
			row := make([]cli.ReportSpan, 0, 4)
			if pr.WillClose {
				row = append(row, cli.Span("fixed by", cli.KindOK))
			} else {
				row = append(row, cli.Span("mentioned by", cli.KindWarn))
			}
			row = append(row, cli.LinkSpan(fmt.Sprintf("PR #%d", pr.RefNumber), f.prHTMLURL(pr.RefNumber)))
			if vs := prVersions[pr.RefNumber]; len(vs) > 0 {
				row = append(row, cli.Span("shipped in v"+vs[0], cli.KindVer))
			} else {
				row = append(row, cli.Span("merged, not yet in a release", cli.KindDim))
			}
			row = append(row, cli.Span("· "+text.OneLine(pr.Title), cli.KindDim))
			item.Evidence = append(item.Evidence, row)
		}
		if len(fdg.bullets) > 0 {
			rep := "version unknown"
			switch {
			case fdg.reported != "":
				rep = "v" + fdg.reported
			case fdg.major > 0:
				rep = fmt.Sprintf("v%d.x", fdg.major)
			}
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("reported against", cli.KindDim), cli.Span(rep, cli.KindVer),
				cli.Span("· uncited fixes shipped since:", cli.KindDim),
			})
			for _, bl := range fdg.bullets {
				kind := cli.KindDim
				if bl.score >= changelogMatchedScore {
					kind = cli.KindOK
				}
				row := []cli.ReportSpan{
					cli.Span("v"+bl.entry.Version, kind),
					cli.Span(text.OneLine(bl.entry.Text), cli.KindQuote),
				}
				if bl.entry.PRNumber != 0 {
					row = append(row, cli.LinkSpan(fmt.Sprintf("PR #%d", bl.entry.PRNumber), f.prHTMLURL(bl.entry.PRNumber)))
				}
				item.Evidence = append(item.Evidence, row)
			}
		}
		if fdg.reopenedBy != 0 {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span(fmt.Sprintf("closed by PR #%d and then reopened — the fix may not have stuck", fdg.reopenedBy), cli.KindBad),
			})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// resolvedReportSection builds the "a linked issue was dealt with" check section.
func (f *Flags) resolvedReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passResolved,
		Name:     "close " + passResolved,
		Question: "a sibling issue covers this one — linked or near-identical, closed or open?",
		Description: "Every open issue another issue answers. Closed targets class by how they were closed — completed " +
			"(with the fixing PR and release when the changelog records them), duplicate, then not planned — linked by " +
			"a crossref, or found by near-identical titles against the entire closed corpus (similar). Open targets " +
			"(open-linked, open-similar) close towards the issue carrying more of the discussion, weighted towards the " +
			"older one. Applying closes pointing at the sibling and its resolution.",
		Command: "koi close resolved [completed|duplicate|not-planned|open|similar] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	findings, counts, _, err := f.collectResolved(d, "")
	if err != nil {
		return s, err
	}
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classCompleted, Count: counts[classCompleted], Kind: cli.KindOK},
		{Name: classDuplicate, Count: counts[classDuplicate], Kind: cli.KindMid},
		{Name: classNotPlanned, Count: counts[classNotPlanned], Kind: cli.KindWarn},
		{Name: linkOpen, Count: counts[linkOpen], Kind: cli.KindMid},
		{Name: classDupSimilar, Count: counts[classDupSimilar], Kind: cli.KindMid},
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.resolvedJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passResolved, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *resolvedFinding) int { return x.issue.Number }, verdicts)
	}

	classKind := map[string]string{classCompleted: cli.KindOK, classDuplicate: cli.KindMid, classNotPlanned: cli.KindWarn}
	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for n := range fdg.targets {
			t := &fdg.targets[n]
			if t.open {
				how, kind := "referenced from this issue — target still OPEN", cli.KindOK
				if t.similarity > 0 {
					how, kind = fmt.Sprintf("%.0f%% title match, nothing links them — target still OPEN", t.similarity*100), cli.KindMid
				}
				item.Evidence = append(item.Evidence, []cli.ReportSpan{
					cli.LinkSpan(fmt.Sprintf("#%d", t.ref.RefNumber), f.issHTMLURL(t.ref.RefNumber)),
					cli.Span(how, kind),
					cli.Span(fmt.Sprintf("opened %s ago · 💬 %d · 👍 %d", text.HumanAge(t.target.CreatedAt, now), t.target.CommentCount, t.target.ThumbsUp), cli.KindDim),
				}, []cli.ReportSpan{cli.Span("“"+text.OneLine(t.ref.Title)+"”", cli.KindQuote)})
				continue
			}
			class := resolvedClass(t.stateReason)
			row := make([]cli.ReportSpan, 0, 6)
			found := cli.Span("links", cli.KindDim)
			if t.similarity > 0 {
				found = cli.Span(fmt.Sprintf("%.0f%% title match, nothing links them —", t.similarity*100), cli.KindMid)
			}
			row = append(row,
				found,
				cli.LinkSpan(fmt.Sprintf("#%d", t.ref.RefNumber), f.issHTMLURL(t.ref.RefNumber)),
				cli.Span("closed "+strings.ReplaceAll(class, "-", " "), classKind[class]))
			if t.closedAt != "" {
				row = append(row, cli.Span(dateOf(t.closedAt), cli.KindDim))
			}
			switch {
			case t.fixPR != 0:
				row = append(row, cli.Span("by", cli.KindDim), cli.LinkSpan(fmt.Sprintf("PR #%d", t.fixPR), f.prHTMLURL(t.fixPR)))
				if t.version != "" {
					row = append(row, cli.Span("in v"+t.version, cli.KindVer))
				} else if t.milestone != "" {
					row = append(row, cli.Span(t.milestone, cli.KindVer))
				}
			case class == classCompleted:
				row = append(row, cli.Span("(no fix recorded)", cli.KindBad))
			}
			row = append(row, cli.Span("· "+text.OneLine(t.ref.Title), cli.KindDim))
			item.Evidence = append(item.Evidence, row)
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// legacyReportSection builds the "this bug is old and unconfirmed" check section.
func (f *Flags) legacyReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	col, err := f.collectLegacy(d, nil)
	if err != nil {
		return cli.ReportSection{Slug: passLegacy}, err
	}
	s := cli.ReportSection{
		Slug:     passLegacy,
		Name:     "close " + passLegacy,
		Question: fmt.Sprintf("this bug is old (v1–v%d) and nobody says it is still alive — close as stale?", col.maxMajor),
		Description: fmt.Sprintf("Open bug and crash reports against legacy majors (v1–v%d) that the keep rules cleared for closing: "+
			"no credible recent-version repro claim, no open linked PR, not highly engaged. Enhancements are not touched. "+
			"Applying closes with the legacy-bug comment, closed as not planned.", col.maxMajor),
		Command: "koi close legacy [--major N] --apply / --apply-with-ai / --apply-with-ai-auto",
		Total:   len(col.findings),
	}
	for m := 1; m <= col.maxMajor; m++ {
		if col.byMajor[m] > 0 {
			s.Classes = append(s.Classes, cli.ReportClass{Name: fmt.Sprintf("v%d.x", m), Count: col.byMajor[m], Kind: cli.KindVer})
		}
	}
	if col.legacyBugs > 0 {
		parts := make([]string, 0, len(col.protected)+1)
		for _, r := range text.SortedKeys(col.protected) {
			parts = append(parts, fmt.Sprintf("%s %d", r, col.protected[r]))
		}
		note := fmt.Sprintf("%d legacy bugs seen in total", col.legacyBugs)
		if len(parts) > 0 {
			note += " · protected from closing: " + strings.Join(parts, " · ")
		}
		if col.diverted > 0 {
			note += fmt.Sprintf(" · %d have a merged PR and belong to koi close fixed", col.diverted)
		}
		s.Note = note
	}

	findings := col.findings
	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.legacyJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passLegacy, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *legacyFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.signals.LastActivity, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		kindTag := cli.KindWarn
		if fdg.signals.Kind == signalKindCrash {
			kindTag = cli.KindBad
		}
		item.Evidence = append(item.Evidence, []cli.ReportSpan{
			cli.Span(fdg.signals.Kind, kindTag),
			cli.Span("on v"+text.OrDefault(fdg.signals.VersionFull, fmt.Sprintf("%d.x", fdg.signals.VersionMajor)), cli.KindVer),
			cli.Span("("+fdg.signals.VersionSource+")", cli.KindDim),
		})
		// a label-sourced quote is just "labelled v/3.x" — the source tag above
		// already says that; only body/template/ai quotes add anything
		if fdg.signals.VersionQuote != "" && fdg.signals.VersionSource != versionSourceLabel {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("version evidence:", cli.KindDim),
				cli.Span("“"+text.TruncateRunes(text.OneLine(fdg.signals.VersionQuote), 120)+"”", cli.KindQuote),
			})
		}

		// the thread's version trail is what decides a staleness close: every
		// version claim with its quote and deep link, or the silence spelt out
		comments, cerr := d.CommentsFor(fdg.issue.Number)
		if cerr != nil {
			return s, cerr
		}
		mentions := issue.VersionMentions(comments)
		const maxMentions = 6
		for n, m := range mentions {
			if n == maxMentions {
				item.Evidence = append(item.Evidence, []cli.ReportSpan{
					cli.Span(fmt.Sprintf("… and %d more version mentions in the thread", len(mentions)-maxMentions), cli.KindDim),
				})
				break
			}
			row := []cli.ReportSpan{
				cli.Span(fmt.Sprintf("v%d.x", m.Major), cli.KindVer),
				cli.Span(fmt.Sprintf("%s ago · @%s:", text.HumanAge(m.At, now), m.Author), cli.KindDim),
				cli.Span("“"+text.TruncateRunes(text.OneLine(m.Quote), 110)+"”", cli.KindQuote),
			}
			if m.URL != "" {
				row = append(row, cli.LinkSpan("view comment", m.URL))
			}
			item.Evidence = append(item.Evidence, row)
		}
		if len(mentions) == 0 && len(comments) > 0 {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span(fmt.Sprintf("no version mentions in the thread's %d comments — nobody re-confirmed on a newer version", len(comments)), cli.KindDim),
			})
		}
		// how the thread ended is the other half of the staleness call
		if len(comments) > 0 {
			last := &comments[len(comments)-1]
			row := []cli.ReportSpan{
				cli.Span(fmt.Sprintf("last comment %s ago · @%s:", text.HumanAge(last.CreatedAt, now), last.Author), cli.KindDim),
				cli.Span("“"+text.TruncateRunes(text.OneLine(issue.CleanBody(last.Body)), 140)+"”", cli.KindQuote),
			}
			if last.URL != "" {
				row = append(row, cli.LinkSpan("view comment", last.URL))
			}
			item.Evidence = append(item.Evidence, row)
		} else {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{cli.Span("no comments at all", cli.KindDim)})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// commentsReportSection builds the "its own thread says it is done" section.
func (f *Flags) commentsReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passComments,
		Name:     "close " + passComments,
		Question: "this issue's own thread says it can be closed — is the claim credible?",
		Description: "Every open issue with a comment claiming it is done: \"this can be closed\", \"fixed in vX by #PR\", " +
			"\"no longer an issue\", or a maintainer saying they will close it. Classes by who says so. " +
			"Applying closes as completed with a comment citing the claim and its author.",
		Command: "koi close comments [maintainer-says|community-says] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	findings, counts, _, err := f.collectComments(d, "")
	if err != nil {
		return s, err
	}
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classMaintainerSays, Count: counts[classMaintainerSays], Kind: cli.KindOK},
		{Name: classCommunitySays, Count: counts[classCommunitySays], Kind: cli.KindWarn},
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.commentsJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passComments, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *commentsFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for n := range fdg.claims {
			cl := &fdg.claims[n]
			authorKind := cli.KindWarn
			assocName := ""
			switch _, label := assocDisplay(cl.comment.AuthorAssociation); {
			case maintainerAssoc(cl.comment.AuthorAssociation):
				authorKind, assocName = cli.KindOK, "("+label+") "
			case label != "":
				authorKind, assocName = "", "("+label+") "
			}
			row := []cli.ReportSpan{
				cli.Span("@"+cl.comment.Author, authorKind),
				cli.Span(assocName+text.HumanAge(cl.comment.CreatedAt, now)+" ago:", cli.KindDim),
				cli.Span("“"+cl.quote+"”", cli.KindQuote),
			}
			if cl.prNumber != 0 {
				row = append(row, cli.LinkSpan(fmt.Sprintf("PR #%d", cl.prNumber), f.prHTMLURL(cl.prNumber)),
					cli.Span("shipped in v"+cl.prVersion, cli.KindVer))
			}
			if cl.comment.URL != "" {
				row = append(row, cli.LinkSpan("view comment", cl.comment.URL))
			}
			item.Evidence = append(item.Evidence, row)
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// questionsReportSection builds the "this question is done with" section.
func (f *Flags) questionsReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passQuestions,
		Name:     "close " + passQuestions,
		Question: "this question was answered, or died unanswered long ago — close it out?",
		Description: "Open question-labelled issues that look done with: answered (a substantive reply exists — the newest, " +
			"maintainers preferred, is the candidate answer — and the thread has settled) or dead (no substantive reply and " +
			"over a year of silence). Answered closes as completed citing the answer; dead closes as not planned pointing " +
			"at the community forum.",
		Command: "koi close questions [answered|dead] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	col, err := f.collectQuestions(d, "")
	if err != nil {
		return s, err
	}
	findings := col.findings
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classQAnswered, Count: col.counts[classQAnswered], Kind: cli.KindOK},
		{Name: classQDead, Count: col.counts[classQDead], Kind: cli.KindWarn},
	}
	if col.questions > 0 {
		s.Note = fmt.Sprintf("%d open question-labelled issues · %d with recent activity · %s",
			col.questions, col.active, keepSummary(col.protected))
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.questionsJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passQuestions, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *questionsFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		if fdg.answer != nil {
			authorKind := cli.KindMid
			assocName := ""
			switch _, label := assocDisplay(fdg.answer.AuthorAssociation); {
			case fdg.answer.IsMaintainer():
				authorKind, assocName = cli.KindOK, "("+label+") "
			case label != "":
				authorKind, assocName = "", "("+label+") "
			}
			row := []cli.ReportSpan{
				cli.Span("candidate answer by", cli.KindDim),
				cli.Span("@"+fdg.answer.Author, authorKind),
				cli.Span(assocName+text.HumanAge(fdg.answer.CreatedAt, now)+" ago:", cli.KindDim),
				cli.Span("“"+text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.answer.Body)), 160)+"”", cli.KindQuote),
			}
			if fdg.answer.URL != "" {
				row = append(row, cli.LinkSpan("view comment", fdg.answer.URL))
			}
			item.Evidence = append(item.Evidence, row)
			if fdg.replies > 1 {
				item.Evidence = append(item.Evidence, []cli.ReportSpan{
					cli.Span(fmt.Sprintf("%d substantive replies in the thread", fdg.replies), cli.KindDim),
				})
			}
		} else {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("no substantive replies at all", cli.KindWarn),
				cli.Span("quiet for "+text.HumanAge(fdg.issue.UpdatedAt, now), cli.KindDim),
			})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// staleReportSection builds the "the maintainer's last word went unanswered"
// section.
func (f *Flags) staleReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passStale,
		Name:     "close " + passStale,
		Question: "a maintainer had the last word and nobody ever answered — close out the thread?",
		Description: "Open issues whose thread ended on a maintainer's comment nobody answered: waiting (labelled " +
			"waiting-response and 90+ days of silence), asked (they requested information that never came; a year of " +
			"silence) or said (they stated a position — by design, API limitation, upstream — nobody disputed for a " +
			"year). A last word that committed to action scores low; the ball stays with the maintainers. Applying " +
			"closes as not planned citing the comment.",
		Command: "koi close stale [waiting|asked|said] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	col, err := f.collectStale(d, "")
	if err != nil {
		return s, err
	}
	findings := col.findings
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classStaleWaiting, Count: col.counts[classStaleWaiting], Kind: cli.KindOK},
		{Name: classStaleAsked, Count: col.counts[classStaleAsked], Kind: cli.KindMid},
		{Name: classStaleSaid, Count: col.counts[classStaleSaid], Kind: cli.KindWarn},
	}
	s.Note = fmt.Sprintf("%d more end on a maintainer's word under a year old · %s", col.recent, keepSummary(col.protected))

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.staleJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passStale, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *staleFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		verb, verbKind := "said", cli.KindWarn
		if fdg.class == classStaleAsked {
			verb, verbKind = "asked", cli.KindOK
		}
		_, label := assocDisplay(fdg.last.AuthorAssociation)
		row := []cli.ReportSpan{
			cli.Span("@"+fdg.last.Author, cli.KindOK),
			cli.Span("("+label+")", cli.KindDim),
			cli.Span(verb, verbKind),
			cli.Span(text.HumanAge(fdg.last.CreatedAt, now)+" ago, unanswered since:", cli.KindDim),
			cli.Span("“"+text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.last.Body)), 200)+"”", cli.KindQuote),
		}
		if fdg.last.URL != "" {
			row = append(row, cli.LinkSpan("view comment", fdg.last.URL))
		}
		item.Evidence = append(item.Evidence, row)
		if fdg.mentionsAuthor {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("addressed the reporter @"+fdg.issue.Author+" directly", cli.KindDim),
			})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// existsReportSection builds the "the ask already exists" section.
func (f *Flags) existsReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passExists,
		Name:     "close " + passExists,
		Question: "this enhancement request appears to already exist in the provider — did it ship?",
		Description: "Open enhancement requests whose ask already exists: the requested resource or data source is in the " +
			"provider docs today and arrived after the request, or a property the request's prose names shipped for one of " +
			"its resources in a later release. Applying closes as completed with the good news and a documentation link.",
		Command: "koi close exists [resource|property] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	findings, counts, _, err := f.collectExists(d, "")
	if err != nil {
		return s, err
	}
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classExistsResource, Count: counts[classExistsResource], Kind: cli.KindOK},
		{Name: classExistsProperty, Count: counts[classExistsProperty], Kind: cli.KindMid},
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.existsJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passExists, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *existsFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for n := range fdg.evidence {
			e := &fdg.evidence[n]
			// evidence is dated when a changelog bullet names it and undated
			// when only the docs do — the row must not claim a release either
			// way it does not have
			var row []cli.ReportSpan
			if e.kind == db.RemovalKindProperty {
				row = []cli.ReportSpan{cli.Span(e.name, ""), cli.Span("on "+e.resource, cli.KindDim)}
				if e.version != "" {
					row = append(row, cli.Span("shipped in v"+e.version, cli.KindVer), cli.LinkSpan(fmt.Sprintf("PR #%d", e.pr), f.prHTMLURL(e.pr)))
				} else {
					row = append(row, cli.Span("in the docs today", cli.KindOK),
						cli.LinkSpan("docs", registryDocURL(e.ownerKind(), e.resource)))
				}
			} else {
				row = []cli.ReportSpan{
					cli.Span(e.name, cli.KindOK), cli.Span("("+strings.ReplaceAll(e.kind, "-", " ")+")", cli.KindDim),
					cli.Span("now exists", cli.KindOK),
				}
				if e.version != "" {
					row = append(row, cli.Span("arrived in v"+e.version, cli.KindVer), cli.LinkSpan(fmt.Sprintf("PR #%d", e.pr), f.prHTMLURL(e.pr)))
				} else {
					row = append(row, cli.Span("— arrival not dated", cli.KindDim))
				}
				row = append(row, cli.LinkSpan("docs", registryDocURL(e.kind, e.name)))
			}
			if e.preAsk {
				row = append(row, cli.Span("(predates the request)", cli.KindDim))
			}
			item.Evidence = append(item.Evidence, row,
				[]cli.ReportSpan{cli.Span("changelog:", cli.KindDim), cli.Span("“"+e.bullet+"”", cli.KindQuote)})
			if e.quote != "" {
				item.Evidence = append(item.Evidence, []cli.ReportSpan{cli.Span("the ask:", cli.KindDim), cli.Span("“"+e.quote+"”", cli.KindQuote)})
			}
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// errorsReportSection builds the "its quoted error is gone from the source"
// section. Unlike every other check it needs a local provider checkout to grep;
// without one configured the section stays empty and says how to enable it.
func (f *Flags) errorsReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passErrors,
		Name:     "close " + passErrors,
		Question: "this bug quotes error output that no longer exists in the provider source — obsolete as written?",
		Description: "Open bug and crash reports whose quoted error or panic output no longer exists anywhere in the provider " +
			"source (vendored SDKs included) — the code that produced it has been rewritten since the report. " +
			"verified means the text existed in the source at the version the issue reported against; text absent there too " +
			"was never the provider's (Azure API responses, Terraform core) and is dropped. " +
			"Applying closes as not planned inviting a fresh issue on the current provider.",
		Command: "koi close errors [verified|panic|unverified] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	// Report validated --provider-src up front, so the checkout is real here
	eo := ErrorsOpts{Src: f.Cmd.Errors.ProviderSrc, Ref: f.Cmd.Errors.ProviderRef}
	col, err := f.collectErrors(d, eo)
	if err != nil {
		return s, err
	}
	findings := col.findings
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classErrVerified, Count: col.counts[classErrVerified], Kind: cli.KindOK},
		{Name: classErrPanic, Count: col.counts[classErrPanic], Kind: cli.KindMid},
		{Name: classErrUnverified, Count: col.counts[classErrUnverified], Kind: cli.KindWarn},
	}
	if col.quoting > 0 {
		s.Note = fmt.Sprintf("%d open bugs/crashes quote error output · %d still in the source · %d never provider text at the reported version · %s",
			col.quoting, col.stillPresent, col.neverFound, keepSummary(col.protected))
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.errorsJudgeItems(d, findings, eo.Ref)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passErrors, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *errorsFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		if fdg.version != "" {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("reported against", cli.KindDim), cli.Span("v"+fdg.version, cli.KindVer),
			})
		}
		for n := range fdg.probes {
			p := &fdg.probes[n]
			kind := "error text"
			if p.frag.Kind == issue.ErrFragPanic {
				kind = "panic function"
			}
			row := []cli.ReportSpan{
				cli.Span(kind, cli.KindDim),
				cli.Span("“"+p.frag.Text+"”", cli.KindQuote),
				cli.Span("gone from "+eo.Ref, cli.KindWarn),
			}
			switch {
			case p.foundAtTag:
				row = append(row, cli.Span("was in the source at "+fdg.tag, cli.KindOK))
			case fdg.tag != "":
				row = append(row, cli.Span("never at "+fdg.tag+" — likely not provider text", cli.KindDim))
			}
			item.Evidence = append(item.Evidence, row)
			if p.frag.Quote != "" {
				src := "from:"
				if p.frag.FromComment {
					src = "from a comment:"
				}
				item.Evidence = append(item.Evidence, []cli.ReportSpan{
					cli.Span(src, cli.KindDim), cli.Span("“"+p.frag.Quote+"”", cli.KindQuote),
				})
			}
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// docsReportSection builds the "its doc page moved on" section. Like the
// errors section it needs the provider checkout; without one configured the
// section stays empty and says how to enable it.
func (f *Flags) docsReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passDocs,
		Name:     "close " + passDocs,
		Question: "this documentation issue's page has been revised since — addressed now?",
		Description: "Open documentation issues whose doc page has been edited since the report. Edits alone prove " +
			"nothing — doc pages churn constantly — so the AI reads the current page content against the issue's " +
			"specific ask. Applying closes as completed pointing at the revised page.",
		Command: "koi close docs --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	// Report validated --provider-src up front, so the checkout is real here
	eo := DocsOpts{Src: f.Cmd.Errors.ProviderSrc, Ref: f.Cmd.Errors.ProviderRef}
	col, err := f.collectDocs(d, eo)
	if err != nil {
		return s, err
	}
	findings := col.findings
	s.Total = len(findings)
	if col.docs > 0 {
		s.Note = fmt.Sprintf("%d open documentation issues · %d with pages untouched since the report · %d whose pages no longer exist · %d naming no known doc page · %s",
			col.docs, col.untouched, col.removed, col.unresolved, keepSummary(col.protected))
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.docsJudgeItems(d, findings, eo.Src, eo.Ref)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocksBatch(d, passDocs, promptText, docsJudgeBatch, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *docsFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for _, p := range fdg.pages {
			row := []cli.ReportSpan{cli.Span(p.name, ""), cli.Span("("+p.kind+")", cli.KindDim)}
			switch {
			case !p.exists:
				row = append(row, cli.Span("page no longer exists", cli.KindBad))
			case p.commits == 0:
				row = append(row, cli.Span("untouched since the report", cli.KindDim))
			default:
				row = append(row,
					cli.Span(fmt.Sprintf("edited %d times since the report", p.commits), cli.KindOK),
					cli.Span("last "+p.lastEdit.Format("2006-01-02"), cli.KindDim),
					cli.LinkSpan("current docs", registryDocURL(p.kind, p.name)))
			}
			item.Evidence = append(item.Evidence, row)
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// deprecatedReportSection builds the "the thing it leans on is gone" section.
func (f *Flags) deprecatedReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     passDeprecated,
		Name:     "close " + passDeprecated,
		Question: "this issue leans on a removed or deprecated resource/property — moot where it stands?",
		Description: "Every open issue referencing a resource, data source, or property that was removed or deprecated, " +
			"per the 4.0/5.0 upgrade guides and the changelog's DEPRECATIONS bullets. " +
			"Applying closes as not planned with a comment naming what is gone, when, and the successor to use.",
		Command: "koi close deprecated [removed-resource|removed-property|...] --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	findings, counts, _, _, err := f.collectDeprecated(d, "")
	if err != nil {
		return s, err
	}
	s.Total = len(findings)
	s.Classes = []cli.ReportClass{
		{Name: classRemovedResource, Count: counts[classRemovedResource], Kind: cli.KindBad},
		{Name: classRemovedProperty, Count: counts[classRemovedProperty], Kind: cli.KindWarn},
		{Name: classDeprecatedResource, Count: counts[classDeprecatedResource], Kind: cli.KindMid},
		{Name: classDeprecatedProperty, Count: counts[classDeprecatedProperty], Kind: cli.KindDim},
	}

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.deprecatedJudgeItems(d, findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passDeprecated, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *deprecatedFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		for n := range fdg.matches {
			m := &fdg.matches[n]
			r := m.removal
			actionKind := cli.KindMid
			if r.Action == db.RemovalRemoved {
				actionKind = cli.KindBad
			}
			row := make([]cli.ReportSpan, 0, 6)
			if r.Kind == db.RemovalKindProperty {
				row = append(row, cli.Span(r.Property, ""), cli.Span("on "+r.Resource, cli.KindDim))
			} else {
				row = append(row, cli.Span(r.Resource, ""), cli.Span("("+strings.ReplaceAll(r.Kind, "-", " ")+")", cli.KindDim))
			}
			if v, ok := strings.CutPrefix(r.Source, "changelog "); ok {
				row = append(row, cli.Span(r.Action, actionKind), cli.LinkSpan("in "+v, removalURL(r)))
			} else {
				row = append(row, cli.Span(r.Action, actionKind), cli.LinkSpan(fmt.Sprintf("in v%d.0 (%s)", r.Major, r.Source), removalURL(r)))
			}
			if r.Successor != "" {
				row = append(row, cli.Span("· use "+r.Successor, cli.KindOK))
			}
			item.Evidence = append(item.Evidence, row)
			if m.quote != "" {
				item.Evidence = append(item.Evidence, []cli.ReportSpan{
					cli.Span("matched:", cli.KindDim), cli.Span("“"+m.quote+"”", cli.KindQuote),
				})
			}
		}
		if len(fdg.alive) > 0 {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("also references (not removed or deprecated):", cli.KindDim),
				cli.Span(strings.Join(fdg.alive, " · "), cli.KindOK),
			})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}

// Import reads a filled-in decisions.csv and records approve/reject decisions.
func (f *Flags) Import(path string) error {
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	file, err := os.Open(path) //nolint:gosec // G304: user-chosen input path is the point
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if len(rows) < 2 {
		return fmt.Errorf("%s has no data rows", path)
	}

	// column positions from the header, so column re-ordering in a spreadsheet is fine
	col := map[string]int{}
	for n, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = n
	}
	numberCol, ok := col["number"]
	if !ok {
		return fmt.Errorf("%s has no 'number' column", path)
	}
	decisionCol, ok := col["decision"]
	if !ok {
		return fmt.Errorf("%s has no 'decision' column", path)
	}

	decider := f.Decider()
	approved, rejected, blank, unknown := 0, 0, 0, 0

	for _, row := range rows[1:] {
		if len(row) <= numberCol || len(row) <= decisionCol {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(row[numberCol]))
		if err != nil {
			continue
		}

		a, err := d.GetAction(number)
		if err != nil {
			return err
		}
		if a == nil || a.Status != db.StatusProposed {
			continue // decided elsewhere, or unknown issue
		}

		switch strings.ToLower(strings.TrimSpace(row[decisionCol])) {
		case "approve", "approved", "yes", "y", "close":
			if err := d.DecideAction(a.ID, db.StatusApproved, decider); err != nil {
				return err
			}
			approved++
		case "reject", "rejected", "no", "n", "keep":
			if err := d.DecideAction(a.ID, db.StatusRejected, decider); err != nil {
				return err
			}
			rejected++
		case "":
			blank++
		default:
			unknown++
			cout.Errorf("  <yellow>#%d:</> unrecognised decision %q\n", number, row[decisionCol])
		}
	}

	cout.Printf("imported as <bold>%s</>: <green>%d approved</> · <red>%d rejected</> · %d blank · %d unrecognised\n",
		decider, approved, rejected, blank, unknown)
	if approved > 0 {
		cout.Printf("next: <cyan>koi apply</>\n")
	}
	return nil
}
