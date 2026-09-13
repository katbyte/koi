package close

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	passResolved   = "resolved"
	promptResolved = "issue-duplicate"

	templateDuplicateResolved = "duplicate-resolved"
	reasonDuplicateResolved   = "duplicate-resolved"

	// classes by how the linked issue was closed, strongest first: a resolved
	// target can cover this issue, a duplicate target chains to whatever it
	// duplicated, a not-planned target resolved nothing.
	classCompleted  = "completed"
	classDuplicate  = "duplicate"
	classNotPlanned = "not-planned"

	// the two classes shared with the open-target flow: linkOpen scopes to
	// duplicates of OPEN issues, classDupSimilar (in duplicates.go) marks
	// pairs nobody linked — here, similarity-found CLOSED targets.
	linkOpen = "open"

	// how many similarity-found closed targets one finding carries — beyond
	// the best two the extra pairs are noise, not evidence
	resolvedSimilarCap = 2
)

// ResolvedOpts configures the resolved audit and its apply modes.
type ResolvedOpts struct {
	Link                string // completed | duplicate | not-planned ("" = every class)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// resolvedTarget is one closed linked issue with everything known about how it
// was dealt with.
type resolvedTarget struct {
	ref         db.Crossref
	stateReason string // COMPLETED | DUPLICATE | NOT_PLANNED | ""
	closedAt    string // when the linked issue was closed, RFC3339 UTC ("" = unknown)
	milestone   string
	fixPR       int     // the PR whose merge closed it (0 = unknown)
	version     string  // earliest release shipping fixPR ("" = unknown)
	similarity  float64 // title overlap for similarity-found targets (0 = crossref-linked)
	shared      []string
	open        bool      // target is still open — the survivor of a duplicate pair
	target      *db.Issue // the live issue for open targets (engagement, dates)
	via         int       // open-chain hop: the linked issue that itself closes towards this one
}

// resolvedFinding is one open issue with its closed same-repo linked issues.
type resolvedFinding struct {
	issue   *db.Issue
	targets []resolvedTarget
	class   string         // strongest target class present
	best    resolvedTarget // the target the close comment cites
}

// Resolved lists every OPEN issue that cross-references a CLOSED issue in the
// same repo — likely duplicates of something already dealt with. Targets class
// by how they were closed: completed (resolved, possibly with a known fix PR
// and release), duplicate, then not-planned. The AI compares the substance of
// both issues before blessing a close; closes comment as a duplicate pointing
// at the linked issue and its resolution.
func (f *Flags) Resolved(link string) error {
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

	o := ResolvedOpts{Link: link, FlagsApplyModes: f.Modes}
	findings, counts, open, err := f.collectResolved(d, o.Link)
	if err != nil {
		return err
	}
	if open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d open issues have a sibling issue that may cover them:</>\n", len(findings), open)
	for _, c := range []struct{ class, tag, note string }{
		{classCompleted, cli.TagGreen, "linked issue closed as completed"},
		{classDuplicate, cli.TagYellow, "linked issue closed as duplicate"},
		{classNotPlanned, cli.TagOrange, "linked issue closed as not planned"},
		{linkOpen, cli.TagLightBlue, "duplicates another OPEN issue that carries the discussion"},
		{classDupSimilar, "lightCyan", "nothing links them, the titles match"},
	} {
		if n := counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-12s</> <yellow>%d</> <gray>(%s)</>\n", c.tag, c.class, n, c.note)
		}
	}
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyResolved(d, findings, o, true)
	case o.Apply:
		return f.applyResolved(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.resolvedJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passResolved, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b resolvedFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without match scores</>\n")
	}

	for n := range findings {
		f.printResolvedCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close resolved --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// collectResolved builds the resolved findings from the crossref cache: every
// open issue referencing a closed same-repo issue, targets enriched from the
// milestone scan (state reason, close time, closing fix PR and its release),
// optionally scoped to one class. Returns findings, per-class counts, and the
// open-issue total.
func (f *Flags) collectResolved(d *db.DB, link string) (findings []resolvedFinding, counts map[string]int, open int, err error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, nil, 0, err
	}
	scanned, err := d.MSIssues()
	if err != nil {
		return nil, nil, 0, err
	}
	msIssues := make(map[int]db.MSIssue, len(scanned))
	for _, m := range scanned {
		msIssues[m.Number] = m
	}
	msFixes, err := d.MSFixesByIssue()
	if err != nil {
		return nil, nil, 0, err
	}
	prVersions, err := d.ChangelogVersionsByPR()
	if err != nil {
		return nil, nil, 0, err
	}

	similar, err := f.resolvedSimilarTargets(d, issues, msIssues, msFixes, prVersions)
	if err != nil {
		return nil, nil, 0, err
	}

	// the open-target sweep (crossrefs between two open issues + title
	// similarity, survivor-weighted, chain-resolved, open-PR protected) —
	// its findings become targets on the same unified list
	dupFindings, _, _, err := f.collectDuplicates(d, "")
	if err != nil {
		return nil, nil, 0, err
	}
	openTargets := map[int][]resolvedTarget{}
	for i := range dupFindings {
		df := &dupFindings[i]
		for n := range df.targets {
			t := &df.targets[n]
			openTargets[df.issue.Number] = append(openTargets[df.issue.Number], resolvedTarget{
				ref:  db.Crossref{IssueNumber: df.issue.Number, RefRepo: f.GH.Repo, RefNumber: t.issue.Number, Title: t.issue.Title},
				open: true, target: t.issue, via: t.via, shared: t.shared,
				similarity: t.similarity,
			})
		}
	}

	counts = map[string]int{}
	for _, i := range issues {
		refs, cerr := d.CrossrefsFor(i.Number)
		if cerr != nil {
			return nil, nil, 0, cerr
		}
		var targets []resolvedTarget
		for _, r := range refs {
			if r.IsPR || r.State != db.IssueClosed || !strings.EqualFold(r.RefRepo, f.GH.Repo) {
				continue
			}
			t := resolvedTarget{ref: r}
			if m, ok := msIssues[r.RefNumber]; ok {
				t.stateReason, t.milestone = m.StateReason, m.Milestone
				if !m.ClosedAt.IsZero() {
					// full precision: the tail split must place same-day
					// comments on the right side of the close
					t.closedAt = m.ClosedAt.UTC().Format(time.RFC3339)
				}
			}
			for _, fx := range msFixes[r.RefNumber] {
				if fx.Link == db.LinkClosedBy {
					t.fixPR = fx.PRNumber
					if vs := prVersions[fx.PRNumber]; len(vs) > 0 {
						t.version = vs[0]
					}
				}
			}
			targets = append(targets, t)
		}
		// similarity-found closed issues ride along as extra targets — unless
		// a crossref already covers that number, in which case the link wins
		linked := map[int]bool{}
		for _, t := range targets {
			linked[t.ref.RefNumber] = true
		}
		for _, t := range similar[i.Number] {
			if !linked[t.ref.RefNumber] {
				targets = append(targets, t)
				linked[t.ref.RefNumber] = true
			}
		}
		for _, t := range openTargets[i.Number] {
			if !linked[t.ref.RefNumber] {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			continue
		}

		fdg := resolvedFinding{issue: i, targets: targets, class: resolvedTargetClass(&targets[0]), best: targets[0]}
		for n := range targets {
			if resolvedRank(targets[n]) > resolvedRank(fdg.best) {
				fdg.class = resolvedTargetClass(&targets[n])
			}
		}
		// the comment cites the strongest target, preferring one with a known fix
		for _, t := range targets {
			cur, cand := resolvedRank(fdg.best), resolvedRank(t)
			if cand > cur || (cand == cur && fdg.best.fixPR == 0 && t.fixPR != 0) {
				fdg.best = t
			}
		}
		if link != "" && fdg.class != link {
			continue
		}
		findings = append(findings, fdg)
		counts[fdg.class]++
	}
	return findings, counts, len(issues), nil
}

// resolvedClass maps a github state reason to a class.
func resolvedClass(stateReason string) string {
	switch stateReason {
	case "COMPLETED":
		return classCompleted
	case "DUPLICATE":
		return classDuplicate
	default:
		return classNotPlanned
	}
}

// resolvedTargetClass is the class one target argues for: similarity-found
// targets are always the similar class — nobody linked them, whatever the
// target's state or outcome — and linked open targets are the open class.
func resolvedTargetClass(t *resolvedTarget) string {
	switch {
	case t.similarity > 0:
		return classDupSimilar
	case t.open:
		return linkOpen
	}
	return resolvedClass(t.stateReason)
}

// resolvedRank orders targets: any human link beats a title match, closed
// outcomes beat an open sibling, and among outcomes completed beats duplicate
// beats not-planned.
func resolvedRank(t resolvedTarget) int {
	switch resolvedTargetClass(&t) {
	case classCompleted:
		return 4
	case classDuplicate:
		return 3
	case classNotPlanned:
		return 2
	case linkOpen:
		return 1
	default:
		return 0
	}
}

// resolvedSimilarTargets pairs open issues against the ENTIRE closed corpus
// from the milestone scan (titles for every issue ever) by the same rules the
// open-target sweep uses: blocked on a shared resource or two distinctive
// title words, kept when the titles really overlap AND both name a resource
// in common. The survivor question does not arise — a closed issue always
// survives — so every hit is a target on the open issue.
func (f *Flags) resolvedSimilarTargets(d *db.DB, issues []*db.Issue, msIssues map[int]db.MSIssue, msFixes map[int][]db.MSFix, prVersions map[int][]string) (map[int][]resolvedTarget, error) {
	openSet := map[int]bool{}
	for _, i := range issues {
		openSet[i.Number] = true
	}

	// token sets over both corpora; df computed across the union so generic
	// words are generic regardless of which side they appear on
	openTokens, closedTokens := map[int]map[string]bool{}, map[int]map[string]bool{}
	df := map[string]int{}
	tokenise := func(title string) map[string]bool {
		set := map[string]bool{}
		for _, w := range dupWord.FindAllString(strings.ToLower(title), -1) {
			if len(w) > 2 {
				set[w] = true
			}
		}
		for w := range set {
			df[w]++
		}
		return set
	}
	for _, i := range issues {
		openTokens[i.Number] = tokenise(i.Title)
	}
	var closedNums []int
	for n, m := range msIssues {
		if m.State != db.IssueClosed || openSet[n] {
			continue
		}
		closedNums = append(closedNums, n)
		closedTokens[n] = tokenise(m.Title)
	}
	generic := float64(len(openTokens)+len(closedTokens)) * deprecatedDFCap
	for _, sets := range []map[int]map[string]bool{openTokens, closedTokens} {
		for n := range sets {
			for w := range sets[n] {
				if float64(df[w]) > generic {
					delete(sets[n], w)
				}
			}
		}
	}

	// resources: the open side has signals, the closed side only its title
	openRes, closedRes := map[int]map[string]bool{}, map[int]map[string]bool{}
	for _, i := range issues {
		set := map[string]bool{}
		sig, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, serr
		}
		if sig != nil {
			for _, r := range sig.Resources {
				set[strings.TrimPrefix(r, "data.")] = true
			}
		}
		for w := range openTokens[i.Number] {
			if strings.HasPrefix(w, "azurerm_") {
				set[w] = true
			}
		}
		openRes[i.Number] = set
	}
	for _, n := range closedNums {
		set := map[string]bool{}
		for _, w := range dupWord.FindAllString(strings.ToLower(msIssues[n].Title), -1) {
			if strings.HasPrefix(w, "azurerm_") {
				set[w] = true
			}
		}
		closedRes[n] = set
	}

	cout.Printf("comparing <yellow>%d</> open issue titles against <yellow>%d</> closed ones...\n", len(issues), len(closedNums))

	// blocking: open×closed within a shared resource or two shared words
	type block struct{ opens, closeds []int }
	byResource, byWord := map[string]*block{}, map[string]*block{}
	add := func(m map[string]*block, key string, n int, closed bool) {
		b := m[key]
		if b == nil {
			b = &block{}
			m[key] = b
		}
		if closed {
			b.closeds = append(b.closeds, n)
		} else {
			b.opens = append(b.opens, n)
		}
	}
	for _, i := range issues {
		for r := range openRes[i.Number] {
			add(byResource, r, i.Number, false)
		}
		for w := range openTokens[i.Number] {
			add(byWord, w, i.Number, false)
		}
	}
	for _, n := range closedNums {
		for r := range closedRes[n] {
			add(byResource, r, n, true)
		}
		for w := range closedTokens[n] {
			add(byWord, w, n, true)
		}
	}
	pairs := map[[2]int]bool{}
	for _, b := range byResource {
		if len(b.opens)+len(b.closeds) > dupResourceCap {
			continue
		}
		for _, o := range b.opens {
			for _, c := range b.closeds {
				pairs[[2]int{o, c}] = true
			}
		}
	}
	wordHits := map[[2]int]int{}
	for _, b := range byWord {
		if len(b.opens)+len(b.closeds) > dupTokenCap {
			continue
		}
		for _, o := range b.opens {
			for _, c := range b.closeds {
				wordHits[[2]int{o, c}]++
			}
		}
	}
	for p, n := range wordHits {
		if n >= 2 {
			pairs[p] = true
		}
	}

	out := map[int][]resolvedTarget{}
	for p := range pairs {
		o, c := p[0], p[1]
		sim, shared := titleOverlap(openTokens[o], closedTokens[c])
		if sim < dupMinSimilarity || !sharesAny(openRes[o], closedRes[c]) {
			continue
		}
		m := msIssues[c]
		t := resolvedTarget{
			ref:        db.Crossref{IssueNumber: o, RefRepo: f.GH.Repo, RefNumber: c, State: db.IssueClosed, Title: m.Title},
			similarity: sim, shared: shared,
			stateReason: m.StateReason, milestone: m.Milestone,
		}
		if !m.ClosedAt.IsZero() {
			t.closedAt = m.ClosedAt.UTC().Format(time.RFC3339)
		}
		for _, fx := range msFixes[c] {
			if fx.Link == db.LinkClosedBy {
				t.fixPR = fx.PRNumber
				if vs := prVersions[fx.PRNumber]; len(vs) > 0 {
					t.version = vs[0]
				}
			}
		}
		out[o] = append(out[o], t)
	}
	for o := range out {
		slices.SortStableFunc(out[o], func(a, b resolvedTarget) int {
			switch {
			case a.similarity > b.similarity:
				return -1
			case a.similarity < b.similarity:
				return 1
			}
			return a.ref.RefNumber - b.ref.RefNumber
		})
		if len(out[o]) > resolvedSimilarCap {
			out[o] = out[o][:resolvedSimilarCap]
		}
	}
	return out, nil
}

// applyResolved is both apply modes on the shared harness: plain --apply
// closes everything listed; --apply-with-ai[-auto] gates each close on the
// judge.
func (f *Flags) applyResolved(d *db.DB, findings []resolvedFinding, o ResolvedOpts, withAI bool) error {
	byNumber := map[int]*resolvedFinding{}
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
			return f.closeOneResolved(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = sourceDuplicates
	p.GateLabel = "match"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as duplicates in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close duplicates the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.resolvedJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passResolved, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneResolved handles one candidate: card, the duplicate comment, and the
// close (or preview under dry-run, or the a/s ask when interactive). Closes as
// completed when the linked issue was resolved, not planned otherwise.
func (f *Flags) closeOneResolved(d *db.DB, repo gh.Repo, fdg *resolvedFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printResolvedCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	// every close here says "this duplicates the sibling" — github's duplicate
	// state is exactly that; the template differs by the sibling's state (a
	// closed one carries its resolution, an open one asks people to follow it)
	stateReason := issue.StateDuplicate
	tmplName, reason := templateDuplicateResolved, reasonDuplicateResolved
	if fdg.best.open {
		tmplName, reason = templateDuplicateOpen, reasonDuplicateOpen
	}
	comment, err := f.renderResolvedComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), tmplName, stateReason)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as a duplicate of <cyan>#%d</>?", fdg.issue.Number, fdg.best.ref.RefNumber), comment, fdg.issue.URL)
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
	if fdg.best.open {
		// the survivor must still be open: closing an issue in favour of one
		// closed mid-run would leave nobody tracking it
		throttle()
		target, terr := repo.GetIssue(fdg.best.ref.RefNumber)
		if terr != nil {
			cout.Errorf("      <red>fetching #%d: %v</>\n", fdg.best.ref.RefNumber, terr)
			return issue.ApplyFailed, nil
		}
		if target.State != cli.RESTStateOpen {
			cout.Printf("      <gray>#%d is closed now — skipped, the closed-target classes cover it next fetch</>\n", fdg.best.ref.RefNumber)
			return issue.ApplySkipped, nil
		}
	}

	throttle()
	if err := repo.CreateComment(fdg.issue.Number, comment); err != nil {
		cout.Errorf("      <red>comment failed: %v</>\n", err)
		return issue.ApplyFailed, nil
	}
	throttle()
	if err := repo.CloseIssue(fdg.issue.Number, stateReason); err != nil {
		cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", stateReason)
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reason)

	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reason,
		StateReason: stateReason, Template: tmplName,
		Evidence:       map[string]string{"duplicate-of": fmt.Sprintf("#%d", fdg.best.ref.RefNumber), evidenceKeyVersion: fdg.best.version},
		Source:         "resolved",
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

// printResolvedCard is one candidate: the open issue, its closed linked issues
// with how each was dealt with, and the AI's score when judged.
func (f *Flags) printResolvedCard(fdg *resolvedFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	for i := range fdg.targets {
		cout.Printf("      %s\n", resolvedTargetLine(&fdg.targets[i]))
	}
	cli.PrintVerdict(v)
}

// resolvedTargetLine renders one closed linked issue: its close reason
// coloured, the fix PR and release when known, and the title.
func resolvedTargetLine(t *resolvedTarget) string {
	var b strings.Builder
	if t.open {
		how := fmt.Sprintf("<%s>referenced from this issue</>", cli.TagGreen)
		if t.similarity > 0 {
			how = fmt.Sprintf("<lightCyan>%.0f%% title match, nothing links them</>", t.similarity*100)
		}
		fmt.Fprintf(&b, "<lightBlue>OPEN</> <cyan>#%d</> %s <gray>· 💬 %d · 👍 %d · %s</>",
			t.ref.RefNumber, how, t.target.CommentCount, t.target.ThumbsUp,
			text.TruncateRunes(text.OneLine(t.ref.Title), 55))
		return b.String()
	}
	class := resolvedClass(t.stateReason)
	tag := map[string]string{classCompleted: cli.TagGreen, classDuplicate: cli.TagYellow, classNotPlanned: cli.TagOrange}[class]
	if t.similarity > 0 {
		fmt.Fprintf(&b, "<lightCyan>similar (%.0f%%)</> <cyan>#%d</> <%s>closed %s</>", t.similarity*100, t.ref.RefNumber, tag, strings.ReplaceAll(class, "-", " "))
	} else {
		fmt.Fprintf(&b, "<gray>links</> <cyan>#%d</> <%s>closed %s</>", t.ref.RefNumber, tag, strings.ReplaceAll(class, "-", " "))
	}
	if t.closedAt != "" {
		fmt.Fprintf(&b, " <gray>%s</>", dateOf(t.closedAt))
	}
	switch {
	case t.fixPR != 0:
		fmt.Fprintf(&b, " <gray>by</> PR <lightCyan>#%d</>", t.fixPR)
		if t.version != "" {
			fmt.Fprintf(&b, " <gray>in</> <lightMagenta>v%s</>", t.version)
		} else if t.milestone != "" {
			fmt.Fprintf(&b, " <gray>·</> <lightMagenta>%s</>", t.milestone)
		}
	case class == classCompleted:
		fmt.Fprintf(&b, " <red>(no fix recorded)</>")
	}
	fmt.Fprintf(&b, " <gray>·</> %s", text.TruncateRunes(text.OneLine(t.ref.Title), 65))
	return b.String()
}

// renderResolvedComment renders the duplicate close comment citing the best
// target: the closed-sibling template carries its resolution, the open-
// sibling one points people at the surviving thread.
func (f *Flags) renderResolvedComment(fdg *resolvedFinding) (string, error) {
	name := templateDuplicateResolved
	if fdg.best.open {
		name = templateDuplicateOpen
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
	if fdg.best.open {
		data = struct {
			Target      int
			TargetTitle string
		}{fdg.best.ref.RefNumber, text.OneLine(fdg.best.ref.Title)}
	} else {
		data = struct {
			Linked       int
			LinkedTitle  string
			Resolved     bool
			Version      string
			CurrentMajor int
		}{fdg.best.ref.RefNumber, text.OneLine(fdg.best.ref.Title), resolvedClass(fdg.best.stateReason) == classCompleted, fdg.best.version, f.CurrentMajor}
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// splitTailAt splits rendered "[RFC3339] author: text" comment lines into
// those at or before the close and those after it, comparing full timestamps
// (both sides UTC, so string order is time order). Lines without a timestamp
// (the truncation note) and everything without a close time count as before.
func splitTailAt(tail, closedAt string) (before, after string) {
	var b, a strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(tail, "\n"), "\n") {
		if line == "" {
			continue
		}
		ts := ""
		if line[0] == '[' {
			if end := strings.IndexByte(line, ']'); end > 1 {
				ts = line[1:end]
			}
		}
		out := &b
		if closedAt != "" && ts > closedAt {
			out = &a
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return b.String(), a.String()
}

// dateOf trims an RFC3339 timestamp to its date for display.
func dateOf(ts string) string {
	if len(ts) > 10 {
		return ts[:10]
	}
	return ts
}

// resolvedJudgeItems fetches the linked issues' texts and renders one judge
// block per finding: both sides' substance plus how each target was closed.
func (f *Flags) resolvedJudgeItems(d *db.DB, findings []resolvedFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptResolved)
	if err != nil {
		return "", nil, err
	}

	// closed targets need their text fetched; open targets are in the db
	targetNumbers := map[int]bool{}
	for i := range findings {
		for _, t := range findings[i].targets {
			if !t.open {
				targetNumbers[t.ref.RefNumber] = true
			}
		}
	}
	if err := f.FetchTexts(d, text.SortedKeys(targetNumbers)); err != nil {
		return "", nil, err
	}
	texts, err := d.Texts()
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
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		// the open issue's own thread often settles it: "fixed by #X" supports
		// the duplicate, "still happening on vY" refutes it
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "ISSUE COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		b.WriteString("SIBLING ISSUES (closed ones with their outcome, open ones with who carries the discussion):\n")
		for _, t := range fdg.targets {
			if t.open {
				how := "referenced from the issue above"
				switch {
				case t.via != 0:
					how = fmt.Sprintf("the open end of the duplicate chain: #%d, which the issue above links, is itself being closed as a duplicate of this one — the close comment would point HERE", t.via)
				case t.similarity > 0:
					how = fmt.Sprintf("NOT linked to it, %.0f%% title overlap (%s)", t.similarity*100, strings.Join(t.shared, ", "))
				}
				side := "older"
				if t.ref.RefNumber > fdg.issue.Number {
					side = "NEWER than the issue above, but carries more of the discussion"
				}
				fmt.Fprintf(&b, "- Issue #%d (STILL OPEN — closing this issue would point people there; %s, %s, opened %s, 💬 %d, 👍 %d): %s\n",
					t.ref.RefNumber, how, side, t.target.CreatedAt.Format("2006-01-02"),
					t.target.CommentCount, t.target.ThumbsUp, text.OneLine(t.ref.Title))
				if t.target.Body != "" {
					fmt.Fprintf(&b, "  THAT ISSUE'S BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(t.target.Body), cli.PRBodyRunes))
				}
				tc, terr := d.CommentsFor(t.ref.RefNumber)
				if terr != nil {
					return "", nil, terr
				}
				if picked := issue.DigestComments(tc, 4); len(picked) > 0 {
					fmt.Fprintf(&b, "  THAT ISSUE'S COMMENTS (%d of %d):\n", len(picked), len(tc))
					for _, c := range picked {
						fmt.Fprintf(&b, "  - [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
							text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
					}
				}
				continue
			}
			how := "closed as " + strings.ReplaceAll(resolvedClass(t.stateReason), "-", " ")
			if t.similarity > 0 {
				how = fmt.Sprintf("NOT linked by anyone — found by title similarity %.0f%% (shared words: %s), %s",
					t.similarity*100, strings.Join(t.shared, " "), how)
			}
			if t.closedAt != "" {
				how += " on " + dateOf(t.closedAt)
			}
			switch {
			case t.fixPR != 0:
				how += fmt.Sprintf(", fixed by PR #%d", t.fixPR)
				if t.version != "" {
					how += ", shipped in v" + t.version
				}
			default:
				how += ", with NO fixing PR or release recorded"
			}
			fmt.Fprintf(&b, "- Issue #%d (%s): %s\n", t.ref.RefNumber, how, text.OneLine(t.ref.Title))
			if txt, ok := texts[t.ref.RefNumber]; ok {
				if txt.Body != "" {
					fmt.Fprintf(&b, "  LINKED ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(txt.Body), cli.PRBodyRunes))
				}
				before, after := splitTailAt(txt.Tail, t.closedAt)
				if before != "" {
					fmt.Fprintf(&b, "  LINKED ISSUE COMMENTS BEFORE THE CLOSE (why it was closed):\n%s", before)
				}
				if after != "" {
					fmt.Fprintf(&b, "  LINKED ISSUE COMMENTS AFTER THE CLOSE (watch for people disputing the closure):\n%s", after)
				}
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// ---- the open-target sweep (once its own duplicates check): pairs of OPEN
// issues that reference each other or share a near-identical title,
// survivor-weighted towards the issue carrying the discussion, chain-resolved
// so nobody is pointed at a thread that is itself closing. ----

const (
	templateDuplicateOpen = "duplicate-open"
	reasonDuplicateOpen   = "duplicate-open"

	// sourceDuplicates is the actions-table source label the standalone
	// duplicates check recorded before it was folded into resolved.
	sourceDuplicates = "duplicates"

	// classes by how the other issue was found, strongest first: this issue
	// says it links the other one, or nobody linked anything and the titles
	// are near-identical.
	classDupLinked  = "linked"
	classDupSimilar = "similar"

	// how much more engagement a newer issue needs before it displaces an
	// older one as the survivor. The older issue has the history, so it keeps
	// the discussion unless the newer one is clearly where people actually are.
	dupAgeBias = 1.25

	// and by this many points as well: 6 engagement against 3 is two people
	// versus one, which says nothing about where the discussion lives.
	dupEngagementMargin = 4

	// how much of two titles' distinctive vocabulary must overlap before the
	// pair is worth judging. 0.5 keeps the near-identical restatements and
	// drops issues that merely share a subject.
	dupMinSimilarity = 0.5

	// blocking guards: a title word or a resource shared by half the backlog
	// pairs everything with everything and says nothing.
	dupResourceCap = 500
	dupTokenCap    = 300
)

// dupWord tokenises a title into lowercase words.
var dupWord = regexp.MustCompile(`[a-z][a-z0-9_]*`)

// duplicateTarget is the older open issue this one appears to duplicate.
type duplicateTarget struct {
	issue      *db.Issue
	class      string
	similarity float64  // title overlap, for the similar class
	shared     []string // the distinctive words both titles use
	via        int      // the linked target whose own close chained here (0 = cited directly)
}

// duplicateFinding is one open issue that appears to duplicate an older one.
type duplicateFinding struct {
	issue   *db.Issue
	targets []duplicateTarget
	class   string
	best    duplicateTarget // the target the close comment cites
}

// collectDuplicates pairs open issues two ways: the crossref cache for issues
// that link each other, and title overlap for the ones nobody ever linked. For
// each pair the issue with less engagement is the candidate to close (see
// dupSurvivor), so the thread people are actually using is the one that stays.
func (f *Flags) collectDuplicates(d *db.DB, link string) (findings []duplicateFinding, counts map[string]int, open int, err error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, nil, 0, err
	}
	counts = map[string]int{}
	if len(issues) == 0 {
		return nil, counts, 0, nil
	}

	byNumber := map[int]*db.Issue{}
	titles := map[int]map[string]bool{}
	df := map[string]int{}
	for _, i := range issues {
		byNumber[i.Number] = i
		set := map[string]bool{}
		for _, w := range dupWord.FindAllString(strings.ToLower(i.Title), -1) {
			if len(w) > 2 {
				set[w] = true
			}
		}
		titles[i.Number] = set
		for w := range set {
			df[w]++
		}
	}
	// a word half the backlog uses ("azurerm", "support", "error") carries no
	// signal about two issues being the same one
	generic := float64(len(issues)) * deprecatedDFCap
	for n, set := range titles {
		for w := range set {
			if float64(df[w]) > generic {
				delete(titles[n], w)
			}
		}
	}

	// targets keyed by the issue that would close, best-first per class
	targets := map[int][]duplicateTarget{}
	addPair := func(a, b *db.Issue, class string, similarity float64, shared []string) {
		if a == nil || b == nil || a.Number == b.Number {
			return
		}
		survivor, closing := dupSurvivor(a, b)
		targets[closing.Number] = append(targets[closing.Number], duplicateTarget{
			issue: survivor, class: class, similarity: similarity, shared: shared,
		})
	}

	// linked: this issue references another open issue. Crossrefs are
	// symmetric on github, so the reference is evidence either way round; the
	// older issue is the survivor regardless of which end wrote the link.
	for _, i := range issues {
		refs, cerr := d.CrossrefsFor(i.Number)
		if cerr != nil {
			return nil, nil, 0, cerr
		}
		for _, r := range refs {
			if r.IsPR || !strings.EqualFold(r.RefRepo, f.GH.Repo) {
				continue
			}
			other := byNumber[r.RefNumber]
			if other == nil {
				continue // closed, or never fetched — koi close resolved owns those
			}
			addPair(i, other, classDupLinked, 0, nil)
		}
	}

	cout.Printf("comparing <yellow>%d</> open issue titles for near-identical wording...\n", len(issues))

	// similar: nobody linked anything, so pair issues that share a resource or
	// two distinctive title words, then keep the pairs whose titles really do
	// overlap. Blocking first keeps this to thousands of comparisons instead
	// of the four and a half million every-pair would need.
	byResource := map[string][]int{}
	byWord := map[string][]int{}
	resources := map[int]map[string]bool{}
	openPRs := map[int]int{}
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, nil, 0, serr
		}
		set := map[string]bool{}
		if s != nil {
			openPRs[i.Number] = s.OpenLinkedPRs
			for _, r := range s.Resources {
				byResource[r] = append(byResource[r], i.Number)
				set[r] = true
			}
		}
		resources[i.Number] = set
		for w := range titles[i.Number] {
			byWord[w] = append(byWord[w], i.Number)
		}
	}

	pairs := map[[2]int]bool{}
	wordHits := map[[2]int]int{}
	skippedBlocks := 0
	for _, nums := range byResource {
		if len(nums) > dupResourceCap {
			skippedBlocks++
			continue
		}
		eachPair(nums, func(a, b int) { pairs[[2]int{a, b}] = true })
	}
	for _, nums := range byWord {
		if len(nums) > dupTokenCap {
			continue
		}
		eachPair(nums, func(a, b int) { wordHits[[2]int{a, b}]++ })
	}
	for p, n := range wordHits {
		if n >= 2 {
			pairs[p] = true
		}
	}
	if skippedBlocks > 0 {
		cout.Printf("<gray>  %d resource(s) appear on more than %d open issues — too broad to pair on, skipped</>\n", skippedBlocks, dupResourceCap)
	}

	sameWordsElsewhere := 0
	for p := range pairs {
		older, newer := p[0], p[1]
		sim, shared := titleOverlap(titles[older], titles[newer])
		if sim < dupMinSimilarity {
			continue
		}
		// both issues must name the same resource. Wording alone pairs every
		// "Provider produced inconsistent final plan" with every other one:
		// that is a terraform error message, not a subject, and two issues that
		// share no resource are not the same issue however alike they read
		if !sharesAny(resources[older], resources[newer]) {
			sameWordsElsewhere++
			continue
		}
		addPair(byNumber[older], byNumber[newer], classDupSimilar, sim, shared)
	}

	if sameWordsElsewhere > 0 {
		cout.Printf("<gray>  %d title match(es) dropped: the two issues name no resource in common</>\n", sameWordsElsewhere)
	}

	protected := 0
	for _, i := range issues {
		ts := targets[i.Number]
		if len(ts) == 0 {
			continue
		}
		// an issue with a PR in flight is being worked on, not duplicated away
		if openPRs[i.Number] > 0 {
			protected++
			continue
		}

		// linked evidence outranks a title match, and among equals the oldest
		// target wins: it is the one carrying the history
		slices.SortStableFunc(ts, func(a, b duplicateTarget) int {
			if a.class != b.class {
				if a.class == classDupLinked {
					return -1
				}
				return 1
			}
			if a.similarity != b.similarity {
				if a.similarity > b.similarity {
					return -1
				}
				return 1
			}
			return a.issue.Number - b.issue.Number
		})
		// keep only the strongest entry per target — CompactFunc would miss
		// non-adjacent repeats, and a pair that is both cross-linked and
		// title-similar would appear twice, contradicting itself in the judge
		// block ("referenced from the issue above" AND "NOT linked to it")
		seenTargets := map[int]bool{}
		ts = slices.DeleteFunc(ts, func(t duplicateTarget) bool {
			if seenTargets[t.issue.Number] {
				return true
			}
			seenTargets[t.issue.Number] = true
			return false
		})

		fdg := duplicateFinding{issue: i, targets: ts, class: ts[0].class, best: ts[0]}
		if link != "" && fdg.class != link {
			continue
		}
		findings = append(findings, fdg)
		counts[fdg.class]++
	}
	if protected > 0 {
		cout.Printf("<gray>  %d skipped: an open PR is already linked to them</>\n", protected)
	}

	// an issue whose survivor is itself being closed as a duplicate would send
	// people to a closed thread, so follow the chain to the one that stays open
	closingTo := map[int]int{}
	for i := range findings {
		closingTo[findings[i].issue.Number] = findings[i].best.issue.Number
	}
	for i := range findings {
		direct := findings[i].best.issue.Number
		seen := map[int]bool{findings[i].issue.Number: true}
		end := findings[i].best
		for {
			next, chained := closingTo[end.issue.Number]
			if !chained || seen[end.issue.Number] {
				break
			}
			seen[end.issue.Number] = true
			target := byNumber[next]
			if target == nil || target.Number == findings[i].issue.Number {
				break // a cycle: leave it, the live guard catches the rest
			}
			end = duplicateTarget{issue: target, class: end.class, similarity: end.similarity, shared: end.shared}
		}
		if end.issue.Number != direct {
			// the comment will cite the chain end, so the judge must score
			// this issue against it too — not just the directly-linked target
			end.via = direct
			cited := end
			if !slices.ContainsFunc(findings[i].targets, func(t duplicateTarget) bool { return t.issue.Number == cited.issue.Number }) {
				findings[i].targets = append([]duplicateTarget{cited}, findings[i].targets...)
			}
		}
		findings[i].best = end
	}

	rank := map[string]int{classDupLinked: 1, classDupSimilar: 0}
	slices.SortStableFunc(findings, func(a, b duplicateFinding) int {
		if d := rank[b.class] - rank[a.class]; d != 0 {
			return d
		}
		if a.best.similarity != b.best.similarity {
			if a.best.similarity > b.best.similarity {
				return -1
			}
			return 1
		}
		return a.issue.Number - b.issue.Number
	})
	return findings, counts, len(issues), nil
}

// eachPair calls fn for every ordered pair in nums, smaller number first.
func eachPair(nums []int, fn func(a, b int)) {
	sorted := slices.Clone(nums)
	slices.Sort(sorted)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] != sorted[j] {
				fn(sorted[i], sorted[j])
			}
		}
	}
}

// dupEngagement is how much of a conversation an issue carries: a thumbs up is
// somebody asking for it, which counts double a comment, since a long thread
// can be two people going back and forth about one confusion.
func dupEngagement(i *db.Issue) int {
	return 2*i.ThumbsUp + i.CommentCount
}

// dupSurvivor picks which issue of a duplicate pair keeps the discussion: the
// one with more engagement, weighted towards the older issue, which has the
// history and needs to be clearly out-participated before it is closed.
func dupSurvivor(a, b *db.Issue) (survivor, closing *db.Issue) {
	older, newer := a, b
	if older.Number > newer.Number {
		older, newer = newer, older
	}
	o, n := dupEngagement(older), dupEngagement(newer)
	if float64(n) > float64(o)*dupAgeBias && n >= o+dupEngagementMargin {
		return newer, older
	}
	return older, newer
}

// sharesAny reports whether two sets have a member in common.
func sharesAny(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// titleOverlap is how much of two titles' distinctive vocabulary is shared,
// with the words that back it up.
func titleOverlap(a, b map[string]bool) (similarity float64, shared []string) {
	if len(a) == 0 || len(b) == 0 {
		return 0, nil
	}
	for w := range a {
		if b[w] {
			shared = append(shared, w)
		}
	}
	slices.Sort(shared)
	union := len(a) + len(b) - len(shared)
	return float64(len(shared)) / float64(union), shared
}
