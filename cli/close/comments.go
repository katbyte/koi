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
	passComments          = "comments"
	promptComments        = "issue-comments-close"
	templateCommentsClose = "comments-close"
	reasonComments        = "closeable-comment"

	// classes by who says it can be closed — a maintainer saying so is close
	// to a decision, the community saying so is a lead.
	classMaintainerSays = "maintainer-says"
	classCommunitySays  = "community-says"
)

// commentsPatterns are the claim shapes worth surfacing. They optimise HARD
// for recall — the AI judges every claim in thread context, so a false
// positive here costs a verdict, not a wrong close; a false negative is a
// closeable issue nobody ever sees. noNeg patterns skip the negation filter
// because their own phrasing is negative ("cannot reproduce").
var commentsPatterns = []struct {
	kind  string
	noNeg bool
	re    *regexp.Regexp
}{
	{"can be closed", false, regexp.MustCompile(`(?:can|could|should|may|might|safe to|ok(?:ay)? to|good to|fine to|feel free to|happy to|time to)\s+(?:be\s+|probably be\s+|just be\s+|now be\s+)?clos(?:e|ed|ing)\b`)},
	{"will close", false, regexp.MustCompile(`(?:going to|gonna|will|about to|let'?s)\s+(?:go ahead and\s+)?close\s+(?:this|it|the issue)|closing this (?:issue|one|out|as)`)},
	{"close this", false, regexp.MustCompile(`(?:please\s+)?close\s+(?:this|it|the)\s+(?:issue|one|ticket)|mark(?:ing)?\s+(?:this\s+|the\s+)?(?:issue\s+)?as\s+(?:closed|resolved|done|completed?)`)},
	{"fixed in", false, regexp.MustCompile(`(?:fixed|resolved|implemented|addressed|released|shipped|solved|added|included|landed|merged|delivered|available|supported)\s+(?:in|with|by|via|as of|since|per|through)\s+(?:\[?v?\d|\[?#\d|pr\b|pull\b|release)`)},
	{"is fixed", false, regexp.MustCompile(`(?:this|it|issue|that|which|bug|problem|error)\s+(?:is now|has (?:now |since )?been|got|was|seems(?: to (?:be|have been))?|appears(?: to (?:be|have been))?|should (?:now )?be|looks? (?:to be|like it'?s|fixed)|is)\s*(?:fixed|resolved|implemented|addressed|solved|sorted|completed?|merged|released|closed elsewhere)`)},
	{"already done", false, regexp.MustCompile(`already\s+(?:fixed|resolved|implemented|supported|possible|available|exists?|done|works|covered|been (?:fixed|implemented|added|resolved|addressed))|is now\s+(?:possible|available|supported|implemented|in the provider|part of)|now\s+(?:supported|available|possible|implemented)\b|(?:feature|this|it)\s+(?:now\s+)?exists\b`)},
	{"no longer", false, regexp.MustCompile(`no longer\s+(?:an issue|relevant|needed|necessary|a problem|applicable|reproducible|happens|occurs|the case|valid|blocked|affected)|(?:isn'?t|not)\s+(?:an issue|a problem|relevant|reproducible|happening)\s+(?:any\s?more|now|these days)|\bobsolete\b|\boutdated\b|superseded by|\bmoot\b|redundant now`)},
	{"cannot reproduce", true, regexp.MustCompile(`(?:can\s?not|can'?t|couldn'?t|could not|unable to|no longer able to|not able to|failed to)\s+(?:reproduce|repro\b|replicate)|(?:doesn'?t|does not|no longer)\s+(?:reproduce|happen|occur)\b`)},
	{"works now", false, regexp.MustCompile(`now (?:it )?works|works? now|working (?:now|again|fine|as expected|correctly)|works (?:fine|again|for me|as expected|correctly|perfectly|ok(?:ay)?|well)(?: now| again)?|works (?:with|on|in|since|as of|after|using) v?\d|(?:issue|problem|error) (?:is |has )?gone\b|went away|resolved itself|behaving (?:correctly|as expected)`)},
}

// commentsAuthorSolved catches the reporter resolving their own issue in
// prose: "my mistake", "user error", "figured it out". Only the issue
// author's words count — anyone else's "my mistake" retracts a comment, not
// the issue — so the loop gates this on the comment author.
var commentsAuthorSolved = regexp.MustCompile(`my (?:mistake|bad|fault)\b|user error|figured (?:it|this) out|solved (?:it|this)\b|never\s?mind\b|(?:mistake|error|problem|issue) (?:was )?on (?:my|our) (?:end|side)|i was doing (?:it|something) wrong|misconfigur\w+ on (?:my|our)`)

// commentsShortWorks catches bare confirmations — a tiny comment that just
// says "works!" or "working" is a thumbs-up, while "works" buried in a long
// comment usually is not.
var commentsShortWorks = regexp.MustCompile(`\bwork(?:s|ing|ed)\b`)

// commentsPRRef finds pull request references: the explicit forms and bare
// #N. Only refs that resolve to a changelog-shipped PR become claims — a
// linked PR that never shipped is work-in-flight, the opposite of closeable.
var commentsPRRef = regexp.MustCompile(`(?:\bpr\s*#?|\bpull request\s*#?|pull/|#)(\d{2,6})\b`)

// commentsNegation rejects claims that are negated, hypothetical, or refuted
// in the immediate context ("can't be closed", "until this is fixed", "still
// an issue"). The AI re-checks the full thread; this just trims obvious noise.
var commentsNegation = regexp.MustCompile(`(?:can'?t|can\s?not|cannot|shouldn'?t|should not|won'?t|not\s+(?:yet\s+)?be|until|unless|once|when|hope|hoping|wish|if)\s+[^.]{0,40}$|still (?:not|an issue|happening|broken|occurs|failing|seeing|getting)|not (?:yet )?(?:fixed|resolved|working)`)

// CommentsOpts configures the closeable audit and its apply modes.
type CommentsOpts struct {
	Link                string // maintainer-says | community-says ("" = both)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// commentsClaim is one comment asserting the issue can be closed.
type commentsClaim struct {
	kind      string // which pattern matched
	comment   db.Comment
	quote     string // the sentence around the match
	prNumber  int    // a linked PR that shipped per the changelog (0 = none)
	prVersion string // the release that shipped it
}

// commentsFinding is one open issue whose thread says it can be closed.
// claims are ordered as found (oldest first); the close comment cites the
// newest maintainer claim, or the newest claim when none is from a maintainer.
type commentsFinding struct {
	issue  *db.Issue
	claims []commentsClaim
	class  string
	best   commentsClaim
}

// assocDisplay returns the author's colour tag and the association label to
// show ("" = say nothing): maintainers green, contributors light blue, plain
// users white with no label — NONE just means no repo affiliation. Orange is
// deliberately avoided: the open-state tag owns it in these cards.
func assocDisplay(assoc string) (tag, label string) {
	switch assoc {
	case "MEMBER", "OWNER", "COLLABORATOR":
		return cli.TagGreen, strings.ToLower(assoc)
	case "CONTRIBUTOR":
		return cli.TagLightBlue, "contributor"
	case "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER":
		return "white", "first-time"
	default:
		return "white", ""
	}
}

// maintainerAssoc reports whether the association carries maintainer weight.
func maintainerAssoc(assoc string) bool {
	return assoc == "MEMBER" || assoc == "OWNER" || assoc == "COLLABORATOR"
}

// Comments finds OPEN issues whose own thread says they are done: comments
// like "this can be closed", "fixed in v3.27.0 by #18588", "no longer an
// issue". The patterns surface the claims; the AI reads each claim in thread
// context (negations, questions, later disputes) before blessing a close.
func (f *Flags) Comments(link string) error {
	o := CommentsOpts{Link: link, FlagsApplyModes: f.Modes}
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

	findings, counts, open, err := f.collectComments(d, o.Link)
	if err != nil {
		return err
	}
	if open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d open issues have a comment saying they can be closed:</>\n", len(findings), open)
	for _, c := range []struct{ class, tag string }{
		{classMaintainerSays, cli.TagGreen}, {classCommunitySays, cli.TagOrange},
	} {
		if n := counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-16s</> <yellow>%d</>\n", c.tag, c.class, n)
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
		return f.applyComments(d, findings, o, true)
	case o.Apply:
		return f.applyComments(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.commentsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passComments, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b commentsFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without claim scores</>\n")
	}

	for n := range findings {
		f.printCommentsCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close comments --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// collectComments scans every open issue's comments for closeable claims.
func (f *Flags) collectComments(d *db.DB, link string) (findings []commentsFinding, counts map[string]int, open int, err error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, nil, 0, err
	}
	prVersions, err := d.ChangelogVersionsByPR()
	if err != nil {
		return nil, nil, 0, err
	}
	cout.Printf("scanning the comments of <yellow>%d</> open issues for closeable claims...\n", len(issues))

	counts = map[string]int{}
	for _, i := range issues {
		comments, cerr := d.CommentsFor(i.Number)
		if cerr != nil {
			return nil, nil, 0, cerr
		}
		fdg := commentsFinding{issue: i, class: classCommunitySays}
		for ci := range comments {
			c := &comments[ci]
			// LowerASCII, not ToLower: byte length is preserved, so offsets
			// found in low slice c.Body correctly — full case folding can
			// shift them (İ, K) and garble the quotes
			low := text.LowerASCII(c.Body)
			claimed := false
			for _, p := range commentsPatterns {
				m := p.re.FindStringIndex(low)
				if m == nil {
					continue
				}
				start := max(0, m[0]-60)
				ctx := low[start:min(len(low), m[1]+60)]
				// a question mark right after the claim is a question, not a claim
				if (!p.noNeg && commentsNegation.MatchString(ctx)) || strings.Contains(low[m[1]:min(len(low), m[1]+15)], "?") {
					continue
				}
				quote := text.TruncateRunes(text.OneLine(c.Body[max(0, m[0]-50):min(len(c.Body), m[1]+90)]), 140)
				fdg.claims = append(fdg.claims, commentsClaim{kind: p.kind, comment: *c, quote: quote})
				claimed = true
				break
			}
			// the reporter's own "my mistake / figured it out" resolves the
			// issue as surely as any fix — author-gated, see commentsAuthorSolved
			if !claimed && c.Author == i.Author {
				if m := commentsAuthorSolved.FindStringIndex(low); m != nil {
					ctx := low[max(0, m[0]-60):min(len(low), m[1]+60)]
					if !commentsNegation.MatchString(ctx) && !strings.Contains(low[m[1]:min(len(low), m[1]+15)], "?") {
						quote := text.TruncateRunes(text.OneLine(c.Body[max(0, m[0]-50):min(len(c.Body), m[1]+90)]), 140)
						fdg.claims = append(fdg.claims, commentsClaim{kind: "author solved", comment: *c, quote: quote})
						claimed = true
					}
				}
			}
			// a tiny comment that just says "works!" is a confirmation
			if !claimed && len(strings.TrimSpace(c.Body)) <= 40 && commentsShortWorks.MatchString(low) && !strings.Contains(low, "?") && !commentsNegation.MatchString(low) {
				fdg.claims = append(fdg.claims, commentsClaim{kind: "works", comment: *c, quote: text.TruncateRunes(text.OneLine(c.Body), 140)})
				claimed = true
			}
			// PR links: only refs resolving to a changelog-shipped PR count —
			// unshipped links are work-in-flight, the opposite of closeable
			prNum, prVer := 0, ""
			var prLoc []int
			for _, m := range commentsPRRef.FindAllStringSubmatchIndex(low, -1) {
				n, aerr := strconv.Atoi(low[m[2]:m[3]])
				if aerr != nil {
					continue
				}
				if vs := prVersions[n]; len(vs) > 0 {
					prNum, prVer, prLoc = n, vs[0], m
					break
				}
			}
			switch {
			case prNum != 0 && claimed:
				// enrich the claim this comment already made
				fdg.claims[len(fdg.claims)-1].prNumber = prNum
				fdg.claims[len(fdg.claims)-1].prVersion = prVer
			case prNum != 0:
				// quote around the ref that actually shipped, not the comment's
				// first ref ("duplicate of #123, fixed by #456" cites #456)
				quote := text.TruncateRunes(text.OneLine(c.Body[max(0, prLoc[0]-60):min(len(c.Body), prLoc[1]+80)]), 140)
				fdg.claims = append(fdg.claims, commentsClaim{kind: "links shipped pr", comment: *c, quote: quote, prNumber: prNum, prVersion: prVer})
				claimed = true
			}
			if claimed && maintainerAssoc(c.AuthorAssociation) {
				fdg.class = classMaintainerSays
			}
		}
		if len(fdg.claims) == 0 {
			continue
		}
		// cite the newest claim, preferring maintainers
		fdg.best = fdg.claims[len(fdg.claims)-1]
		for _, cl := range slices.Backward(fdg.claims) {
			if maintainerAssoc(cl.comment.AuthorAssociation) {
				fdg.best = cl
				break
			}
		}
		if link != "" && fdg.class != link {
			continue
		}
		findings = append(findings, fdg)
		counts[fdg.class]++
	}

	// maintainer-blessed first, then by issue number
	rank := map[string]int{classMaintainerSays: 1, classCommunitySays: 0}
	slices.SortStableFunc(findings, func(a, b commentsFinding) int {
		if d := rank[b.class] - rank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return findings, counts, len(issues), nil
}

// applyComments is both apply modes on the shared harness: plain --apply
// closes everything listed; --apply-with-ai[-auto] gates each close on the
// judge.
func (f *Flags) applyComments(d *db.DB, findings []commentsFinding, o CommentsOpts, withAI bool) error {
	byNumber := map[int]*commentsFinding{}
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
			return f.closeOneComments(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "issues their threads call done"
	p.GateLabel = "claim"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as completed in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.commentsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passComments, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneComments handles one candidate: card, the comment citing the claim,
// and the close as completed (or preview under dry-run, or the a/s ask).
func (f *Flags) closeOneComments(d *db.DB, repo gh.Repo, fdg *commentsFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printCommentsCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := f.renderCommentsComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateCommentsClose, issue.StateCompleted)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as its thread says?", fdg.issue.Number), comment, fdg.issue.URL)
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
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reasonComments)

	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reasonComments,
		StateReason: issue.StateCompleted, Template: templateCommentsClose,
		Evidence: map[string]string{
			"claim": text.TruncateRunes(fdg.best.quote, 120), "author": fdg.best.comment.Author,
			"comment-url": fdg.best.comment.URL,
		},
		Source:         passComments,
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

// closeableVersion pulls a release-looking version out of the cited claim, so
// the close comment can name it.
var reClaimVersion = regexp.MustCompile(`v?(\d+\.\d+(?:\.\d+)?)`)

// renderCommentsComment renders the close comment citing the best claim.
func (f *Flags) renderCommentsComment(fdg *commentsFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateCommentsClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateCommentsClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateCommentsClose, err)
	}
	// the shipped PR's release is authoritative; regexing the quote is the
	// fallback, and its leftmost match can grab the wrong number ("on
	// terraform 1.5.7 this is fixed in azurerm 3.71.0" → 1.5.7)
	version := fdg.best.prVersion
	if version == "" {
		if m := reClaimVersion.FindStringSubmatch(fdg.best.quote); m != nil {
			version = m[1]
		}
	}
	data := struct {
		Author       string
		URL          string
		Version      string
		NoRepro      bool // the cited claim is "cannot reproduce", not "fixed"
		CurrentMajor int
	}{fdg.best.comment.Author, fdg.best.comment.URL, version, fdg.best.kind == "cannot reproduce", f.CurrentMajor}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateCommentsClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// commentsJudgeItems renders one judge block per finding: the issue's body,
// every closeable claim with author standing and date, and a digest of the
// rest of the thread so the AI can spot later disputes.
func (f *Flags) commentsJudgeItems(d *db.DB, findings []commentsFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptComments)
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
		b.WriteString("CLOSEABLE CLAIMS IN THE THREAD:\n")
		for _, cl := range fdg.claims {
			fmt.Fprintf(&b, "- [%s] %s (%s), \"%s\" pattern: %s\n",
				cl.comment.CreatedAt.Format("2006-01-02"), cl.comment.Author, cl.comment.AuthorAssociation,
				cl.kind, text.TruncateRunes(text.OneLine(issue.CleanBody(cl.comment.Body)), 500))
			if cl.prNumber != 0 {
				fmt.Fprintf(&b, "  LINKS PR #%d, which SHIPPED in v%s per the changelog\n", cl.prNumber, cl.prVersion)
			}
		}
		if picked := issue.DigestComments(comments, 10); len(picked) > 0 {
			fmt.Fprintf(&b, "THREAD DIGEST (%d of %d comments — watch for disputes AFTER the claims):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printCommentsCard is one candidate: the issue, each claim with its author's
// standing and a deep link, and the AI's score when judged.
func (f *Flags) printCommentsCard(fdg *commentsFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	shown := 0
	for _, cl := range fdg.claims {
		if shown == 4 {
			cout.Printf("      <gray>… and %d more claims</>\n", len(fdg.claims)-shown)
			break
		}
		shown++
		assocTag, assocName := assocDisplay(cl.comment.AuthorAssociation)
		who := fmt.Sprintf("<%s>%s</>", assocTag, cl.comment.Author)
		if assocName != "" {
			who += fmt.Sprintf(" <gray>(%s)</>", assocName)
		}
		pr := ""
		if cl.prNumber != 0 {
			pr = fmt.Sprintf(" <gray>→</> PR <lightCyan>#%d</> <gray>shipped in</> <lightMagenta>v%s</>", cl.prNumber, cl.prVersion)
		}
		cout.Printf("      %s <gray>%s ·</> <gray>“</>%s<gray>”</>%s <darkGray>%s</>\n",
			who, cl.comment.CreatedAt.Format("2006-01-02"), cl.quote, pr, cl.comment.URL)
	}
	cli.PrintVerdict(v)
}
