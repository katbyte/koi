package close

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/katbyte/koi/cli"

	"golang.org/x/term"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// Classes by how sure we are the quoted output was ever the provider's,
// strongest first: verified means the fragment existed in the source at the
// version the issue reported against; a panic function is provider-specific by
// construction; unverified means it is gone today but the reported version
// could not be checked, so the text may never have been the provider's.
const (
	passErrors          = "errors"
	promptErrors        = "issue-error-gone"
	templateErrorsClose = "errors-close"
	reasonErrors        = "error-gone"

	classErrVerified   = "verified"
	classErrPanic      = "panic"
	classErrUnverified = "unverified"

	// errGrepWorkers is how many git greps run at once: each takes ~half a
	// second over the provider tree and a scan probes hundreds of fragments.
	errGrepWorkers = 8
)

var errClassRank = map[string]int{classErrVerified: 2, classErrPanic: 1, classErrUnverified: 0}

// ErrorsOpts configures the gone-errors audit and its apply modes.
type ErrorsOpts struct {
	Link                string // verified | panic | unverified ("" = all)
	Src                 string // local provider checkout to search
	Ref                 string // git ref treated as the current source
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// errorsProbe is one fragment's fate against the provider source.
type errorsProbe struct {
	frag       issue.ErrorFragment
	foundNow   bool
	foundAtTag bool
}

// errorsFinding is one open issue whose quoted error output no longer exists
// in the provider source. Every probe is gone from the current ref (an issue
// with any still-present fragment is skipped — its code path lives); best is
// the strongest gone fragment, which the close comment cites.
type errorsFinding struct {
	issue   *db.Issue
	sig     *db.Signals
	probes  []errorsProbe
	best    errorsProbe
	version string // reported provider version (signals, or re-parsed from the body)
	tag     string // checkout tag matching it ("" = none)
	class   string
}

// Errors finds OPEN bug and crash reports whose quoted error or panic output
// no longer exists anywhere in the provider source (vendored SDKs included) —
// the code that produced it has been rewritten or removed since the report.
// The AI judges whether each report is really obsolete as written (provider
// wording vs Azure API noise, later still-happening claims); the apply modes
// close as not planned inviting a re-test on the current provider.
func (f *Flags) Errors(link string) error {
	o := ErrorsOpts{Link: link, Src: f.Cmd.Errors.ProviderSrc, Ref: f.Cmd.Errors.ProviderRef, FlagsApplyModes: f.Modes}
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

	col, err := f.collectErrors(d, o)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d of %d open bugs/crashes quote error output that is gone from the provider source:</>\n", len(findings), col.quoting)
	for _, c := range []struct{ class, tag, desc string }{
		{classErrVerified, cli.TagRed, "was in the source at the reported version, gone now"},
		{classErrPanic, cli.TagOrange, "panicking provider function no longer exists"},
		{classErrUnverified, cli.TagYellow, "gone now, reported version unverifiable"},
	} {
		if n := col.counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-12s</> <yellow>%d</>  <gray>%s</>\n", c.tag, c.class, n, c.desc)
		}
	}
	cout.Printf("  <gray>skipped: %d still in the source · %d never provider text at the reported version · %s</>\n",
		col.stillPresent, col.neverFound, keepSummary(col.protected))
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyErrors(d, findings, o, true)
	case o.Apply:
		return f.applyErrors(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest-obsolete first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.errorsJudgeItems(d, findings, o.Ref)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passErrors, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b errorsFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without obsolete scores</>\n")
	}

	for n := range findings {
		f.printErrorsCard(&findings[n], n+1, len(findings), o.Ref, verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close errors --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// errorsCollection is everything collectErrors learns in one scan.
type errorsCollection struct {
	findings     []errorsFinding
	counts       map[string]int
	open         int            // open issues in the db
	quoting      int            // open bugs/crashes with searchable error output
	stillPresent int            // a fragment still exists at the current ref
	neverFound   int            // reported version checkable, fragment never there — not provider text
	protected    map[string]int // keep guards by reason (open PR, current-major claim...)
}

// collectErrors extracts fragments from every open bug/crash body and probes
// them against the provider checkout: first every distinct fragment against
// the current ref (any hit means the code path lives — skip), then the
// survivors against the tag of the version each issue reported, which sorts
// provider-origin text (verified) from Azure API noise (never found).
func (f *Flags) collectErrors(d *db.DB, o ErrorsOpts) (*errorsCollection, error) {
	col := &errorsCollection{counts: map[string]int{}, protected: map[string]int{}}
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	col.open = len(issues)
	if col.open == 0 {
		return col, nil
	}

	g, tags, err := newSrcGrep(o.Src, o.Ref)
	if err != nil {
		return nil, err
	}

	type target struct {
		issue   *db.Issue
		sig     *db.Signals
		frags   []issue.ErrorFragment
		version string
		tag     string
	}
	var targets []*target
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, serr
		}
		if s == nil {
			s = &db.Signals{IssueNumber: i.Number}
		}
		// bugs, crashes, and the unlabelled — a request or question quoting an
		// error is not obsoleted by that error's wording changing
		switch s.Kind {
		case signalKindBug, signalKindCrash, "":
		default:
			continue
		}
		// the error output often lands in a comment (maintainers ask for it),
		// so comments feed the extraction too — the body wins the slots
		comments, cerr := d.CommentsFor(i.Number)
		if cerr != nil {
			return nil, cerr
		}
		commentBodies := make([]string, 0, len(comments))
		for _, c := range comments {
			commentBodies = append(commentBodies, c.Body)
		}
		frags := issue.ExtractErrorFragments(i.Body, commentBodies...)
		if len(frags) == 0 {
			continue
		}
		col.quoting++
		// keep guards, scoped to this check: only the CURRENT major protects.
		// The previous major is no longer maintained (kt 2026-08-31), so unlike
		// the shared rules engine its reports and claims do not shield an issue
		// whose error text is gone — the judge still weighs recent claims.
		vCur := fmt.Sprintf("v%d", f.CurrentMajor)
		switch {
		case s.OpenLinkedPRs > 0:
			col.protected["open-pr"]++
			continue
		case i.ThumbsUp >= f.KeepReactions:
			col.protected["high-engagement"]++
			continue
		case s.NewestClaimMajor >= f.CurrentMajor:
			col.protected["claims-"+vCur]++
			continue
		case s.VersionMajor >= f.CurrentMajor:
			col.protected["reports-"+vCur]++
			continue
		}
		// signals lose the exact version when a v/N.x label wins precedence;
		// the template block in the body usually still has it, and only an
		// exact version gives a tag to verify the fragments against
		version := s.VersionFull
		tag := errTagFor(version, tags)
		if tag == "" {
			if vm := issue.ExtractProviderVersion(i.Body); vm != nil {
				if t := errTagFor(vm.Full, tags); t != "" {
					version, tag = vm.Full, t
				} else if version == "" {
					version = vm.Full
				}
			}
		}
		targets = append(targets, &target{issue: i, sig: s, frags: frags, version: version, tag: tag})
	}

	// phase 1: every distinct fragment against the current ref
	nowJobs := map[string]bool{}
	for _, t := range targets {
		for _, fr := range t.frags {
			nowJobs[fr.Text] = true
		}
	}
	jobs := make([]errGrepJob, 0, len(nowJobs))
	for _, frag := range text.SortedKeys(nowJobs) {
		jobs = append(jobs, errGrepJob{ref: o.Ref, frag: frag})
	}
	cout.Printf("probing <yellow>%d</> fragments from <yellow>%d</> issues against <cyan>%s</> at <lightMagenta>%s</>...\n",
		len(jobs), len(targets), o.Src, o.Ref)
	if err := g.probeParallel(jobs); err != nil {
		return nil, err
	}

	// phase 2: for issues with nothing left at the current ref, the survivors
	// against the reported version's tag
	seen := map[string]bool{}
	jobs = jobs[:0]
	tags2 := map[string]bool{}
	for _, t := range targets {
		alive := false
		for _, fr := range t.frags {
			if found, _ := g.found(o.Ref, fr.Text); found {
				alive = true
				break
			}
		}
		if alive || t.tag == "" {
			continue
		}
		tags2[t.tag] = true
		for _, fr := range t.frags {
			if key := t.tag + "\x00" + fr.Text; !seen[key] {
				seen[key] = true
				jobs = append(jobs, errGrepJob{ref: t.tag, frag: fr.Text})
			}
		}
	}
	if len(jobs) > 0 {
		cout.Printf("verifying <yellow>%d</> gone fragments against <yellow>%d</> reported-version tags...\n", len(jobs), len(tags2))
		if err := g.probeParallel(jobs); err != nil {
			return nil, err
		}
	}

	for _, t := range targets {
		fdg := errorsFinding{issue: t.issue, sig: t.sig, version: t.version, tag: t.tag}
		alive := false
		for _, fr := range t.frags {
			p := errorsProbe{frag: fr}
			p.foundNow, _ = g.found(o.Ref, fr.Text)
			if !p.foundNow && t.tag != "" {
				p.foundAtTag, _ = g.found(t.tag, fr.Text)
			}
			alive = alive || p.foundNow
			fdg.probes = append(fdg.probes, p)
		}
		if alive {
			col.stillPresent++
			continue
		}

		// best: a tag-verified fragment (longest wins), else the panic function,
		// else the longest unverifiable fragment when there was no tag to check
		for _, p := range fdg.probes {
			switch {
			case p.foundAtTag && (!fdg.best.foundAtTag || len(p.frag.Text) > len(fdg.best.frag.Text)):
				fdg.best, fdg.class = p, classErrVerified
			case fdg.class == classErrVerified:
			case p.frag.Kind == issue.ErrFragPanic && t.tag == "" && fdg.class != classErrPanic:
				fdg.best, fdg.class = p, classErrPanic
			case fdg.class == classErrPanic:
			case t.tag == "" && len(p.frag.Text) > len(fdg.best.frag.Text):
				fdg.best, fdg.class = p, classErrUnverified
			}
		}
		if fdg.class == "" {
			// a tag existed and no fragment was ever in the source there — the
			// quoted text was never the provider's (API responses, core output)
			col.neverFound++
			continue
		}
		if o.Link != "" && fdg.class != o.Link {
			continue
		}
		col.findings = append(col.findings, fdg)
		col.counts[fdg.class]++
	}

	slices.SortStableFunc(col.findings, func(a, b errorsFinding) int {
		if d := errClassRank[b.class] - errClassRank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return col, nil
}

// applyErrors is both apply modes on the shared harness: plain --apply closes
// everything listed (the raw evidence cannot tell provider wording from API
// noise, so it exists for pattern consistency); --apply-with-ai[-auto] gates
// each close on the judge and is the recommended path.
func (f *Flags) applyErrors(d *db.DB, findings []errorsFinding, o ErrorsOpts, withAI bool) error {
	byNumber := map[int]*errorsFinding{}
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
			return f.closeOneErrors(d, repo, byNumber[n], v, pos, total, o.Ref, throttle, interactive)
		})
	p.Noun = "issues quoting error output the source has moved past"
	p.GateLabel = "obsolete"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as not planned in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.errorsJudgeItems(d, findings, o.Ref)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passErrors, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneErrors handles one candidate: card, the errors-close comment citing
// the gone fragment, and the close as not planned (or preview under dry-run,
// or the a/s ask when interactive).
func (f *Flags) closeOneErrors(d *db.DB, repo gh.Repo, fdg *errorsFinding, v *issue.Verdict, pos, total int, ref string, throttle func(), ask bool) (int, error) {
	f.printErrorsCard(fdg, pos, total, ref, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := f.renderErrorsComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateErrorsClose, issue.StateNotPlanned)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as obsolete?", fdg.issue.Number), comment, fdg.issue.URL)
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
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reasonErrors)

	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reasonErrors,
		StateReason: issue.StateNotPlanned, Template: templateErrorsClose,
		Evidence: map[string]string{
			"fragment": fdg.best.frag.Text, "kind": fdg.best.frag.Kind,
			evidenceKeyClass: fdg.class, "ref": ref, "tag": fdg.tag,
		},
		Source:         passErrors,
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

// renderErrorsComment renders the close comment citing the strongest gone
// fragment.
func (f *Flags) renderErrorsComment(fdg *errorsFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateErrorsClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateErrorsClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateErrorsClose, err)
	}
	data := struct {
		IsPanic      bool
		Fragment     string
		Tag          string // reported version's tag the fragment was verified at
		CurrentMajor int
	}{fdg.best.frag.Kind == issue.ErrFragPanic, fdg.best.frag.Text, "", f.CurrentMajor}
	if fdg.best.foundAtTag {
		data.Tag = fdg.tag
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateErrorsClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// errorsJudgeItems renders one judge block per finding: the issue's substance
// (body + comment digest), the reported version, and every fragment with its
// verification status, so the AI can tell provider wording from API noise and
// spot still-happening claims.
func (f *Flags) errorsJudgeItems(d *db.DB, findings []errorsFinding, ref string) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptErrors)
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
		fmt.Fprintf(&b, "opened %s, last activity %s\n", fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		if fdg.version != "" {
			fmt.Fprintf(&b, "reported provider version: v%s\n", fdg.version)
		} else if fdg.sig.VersionMajor > 0 {
			fmt.Fprintf(&b, "reported provider version: v%d.x\n", fdg.sig.VersionMajor)
		}
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "ISSUE COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		fmt.Fprintf(&b, "QUOTED OUTPUT SEARCHED IN THE PROVIDER SOURCE (none of it exists at %s):\n", ref)
		for _, p := range fdg.probes {
			status := "UNVERIFIED (reported version could not be checked)"
			switch {
			case p.foundAtTag:
				status = "VERIFIED — was in the source at " + fdg.tag
			case p.frag.Kind == issue.ErrFragPanic:
				status = "PANIC FUNCTION — a provider function the panic stack names"
			case fdg.tag != "":
				status = fmt.Sprintf("never in the source at %s either — likely not provider text", fdg.tag)
			}
			fmt.Fprintf(&b, "- %s `%s`: %s\n", p.frag.Kind, p.frag.Text, status)
			if p.frag.Quote != "" {
				src := "FROM ISSUE LINE"
				if p.frag.FromComment {
					src = "FROM A COMMENT"
				}
				fmt.Fprintf(&b, "  %s: %s\n", src, p.frag.Quote)
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printErrorsCard is one candidate: the issue, each gone fragment with its
// verification status, and the AI's score when judged.
func (f *Flags) printErrorsCard(fdg *errorsFinding, pos, total int, ref string, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	if fdg.version != "" {
		cout.Printf("      <gray>reported against</> <lightMagenta>v%s</>\n", fdg.version)
	}
	for _, p := range fdg.probes {
		kind := "error text"
		if p.frag.Kind == issue.ErrFragPanic {
			kind = "panic in"
		}
		status := fmt.Sprintf("<%s>gone</> <gray>from</> <lightMagenta>%s</>", cli.TagYellow, ref)
		switch {
		case p.foundAtTag:
			status += fmt.Sprintf(" <gray>·</> <%s>was present at</> <lightMagenta>%s</>", cli.TagRed, fdg.tag)
		case fdg.tag != "":
			status += fmt.Sprintf(" <gray>· never at %s</>", fdg.tag)
		}
		cout.Printf("      <gray>%s</> <lightCyan>%s</> %s\n", kind, p.frag.Text, status)
		if p.frag.Quote != "" {
			src := "from:"
			if p.frag.FromComment {
				src = "from a comment:"
			}
			cout.Printf("        <gray>%s</> %s\n", src, text.TruncateRunes(p.frag.Quote, 110))
		}
	}
	cli.PrintVerdict(v)
}

// srcGrep runs fixed-string searches over a provider checkout at a ref via
// git grep, caching every probe — fragments repeat across issues and phases.
type srcGrep struct {
	src   string
	mu    sync.Mutex
	cache map[string]bool
}

// reErrVersion is a reported version a checkout tag can be derived from;
// label-derived values like "3.x" have no tag.
var reErrVersion = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)

// verifyProviderSrc validates the checkout and ref the provider-source checks
// (errors, docs) grep and read.
func verifyProviderSrc(src, ref string) error {
	if src == "" {
		return errors.New("no provider checkout to search — set --provider-src (or provider-src in .koi) to a local clone of the provider")
	}
	git := exec.CommandContext(context.Background(), "git", "-C", src, "rev-parse", "-q", "--verify", ref+"^{commit}") //nolint:gosec // G204: the checkout and ref are user-configured on purpose (--provider-src/-ref)
	if out, err := git.CombinedOutput(); err != nil {
		return fmt.Errorf("resolving --provider-ref %q in %s: %w: %s — pass a ref that exists there (e.g. HEAD)", ref, src, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// newSrcGrep validates the checkout and ref and loads its tag list.
func newSrcGrep(src, ref string) (*srcGrep, map[string]bool, error) {
	if err := verifyProviderSrc(src, ref); err != nil {
		return nil, nil, err
	}
	out, err := exec.CommandContext(context.Background(), "git", "-C", src, "tag", "-l").Output() //nolint:gosec // G204: the checkout is user-configured on purpose (--provider-src)
	if err != nil {
		return nil, nil, fmt.Errorf("listing tags in %s: %w", src, err)
	}
	tags := map[string]bool{}
	for t := range strings.SplitSeq(string(out), "\n") {
		if t = strings.TrimSpace(t); t != "" {
			tags[t] = true
		}
	}
	return &srcGrep{src: src, cache: map[string]bool{}}, tags, nil
}

// errTagFor maps a reported provider version to a checkout tag: 3.71.0 →
// v3.71.0, 4.57 → v4.57.0 ("" when no tag can be derived or none exists).
func errTagFor(full string, tags map[string]bool) string {
	m := reErrVersion.FindStringSubmatch(full)
	if m == nil {
		return ""
	}
	tag := "v" + full
	if m[1] == "" {
		tag += ".0"
	}
	if !tags[tag] {
		return ""
	}
	return tag
}

// found reports whether the fragment exists in any .go file (vendor included —
// message text often lives in the vendored SDK's id types) at the ref.
func (g *srcGrep) found(ref, frag string) (bool, error) {
	key := ref + "\x00" + frag
	g.mu.Lock()
	v, ok := g.cache[key]
	g.mu.Unlock()
	if ok {
		return v, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", g.src, "grep", "-I", "-q", "-F", "-e", frag, ref, "--", "*.go") //nolint:gosec // G204: the checkout and ref are user-configured, the fragment is -F fixed-string data
	cmd.Stderr = &stderr
	err := cmd.Run()
	found := err == nil
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 1 {
			return false, fmt.Errorf("git grep in %s at %s: %w: %s", g.src, ref, err, strings.TrimSpace(stderr.String()))
		}
	}
	g.mu.Lock()
	g.cache[key] = found
	g.mu.Unlock()
	return found, nil
}

// errGrepJob is one (ref, fragment) probe.
type errGrepJob struct{ ref, frag string }

// probeParallel resolves every job through the cache with a small worker
// pool, repainting an in-place done/total counter as greps complete and
// stamping the elapsed time on it at the end; stops at the first real failure.
func (g *srcGrep) probeParallel(jobs []errGrepJob) error {
	started := time.Now()
	sem := make(chan struct{}, errGrepWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	done := 0
	// \r repaints only make sense on a live terminal; piped output gets the
	// header and the final stamped line only
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	for _, j := range jobs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			stop := firstErr != nil
			mu.Unlock()
			if stop {
				return
			}
			_, err := g.found(j.ref, j.frag)
			mu.Lock()
			switch {
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			default:
				done++
				if tty {
					cout.Printf("\r  <yellow>%d</>/<yellow>%d</> probed", done, len(jobs))
				}
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	if firstErr != nil {
		if tty {
			cout.Printf("\n")
		}
		return firstErr
	}
	cout.Printf("\r  <yellow>%d</>/<yellow>%d</> probed <gray>(%s)</>\n", done, len(jobs), time.Since(started).Round(time.Second))
	return nil
}
