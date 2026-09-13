// The question labeller: the `question` label for open issues whose title or
// body reads as an ask but whose labels don't say so.

package label

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

const (
	passLabelQuestion   = "label-question"
	promptLabelQuestion = "issue-question-label"
	labelQuestion       = "question"

	// evidence sources
	questionSourceTitle = "title"
	questionSourceBody  = "body"
)

// questionEvidence is one reason the label is proposed: a question-shaped
// title or a question phrase in the body.
type questionEvidence struct {
	quote  string
	source string // "title" or "body"
	weak   bool   // loose sweep (interrogative lead, "how to") — a lead, not an ask
}

// questionFinding is one open issue that reads as a question but carries no
// question label.
type questionFinding struct {
	issue    *db.Issue
	kind     string // the kind its labels already claim ("" = none)
	evidence []questionEvidence
}

// reQuestionLead matches text that opens interrogatively. The match alone is
// not enough — "Cannot create X" opens with "cannot" not "can", but "Can not
// import Y" would slip through, so questionLead checks the second word.
var reQuestionLead = regexp.MustCompile(`(?i)^(?:how|why|what|when|where|which|who|can|could|should|would|does|do|did|is|are|am|will)\s+(\S+)`)

// reQuestionPhrase finds phrases someone asking (not reporting) writes —
// recall is the sweep's job, the judge's is precision, so bug-report shapes
// slip through by design.
var reQuestionPhrase = regexp.MustCompile(`(?i)(?:is it possible|how (?:do|can|would|should) (?:i|we|you|one)\b|is there a(?:ny)? (?:way|method|option)\b|any way to\b|am i missing\b|what am i doing wrong|wondering if\b|what(?:'s| is) the (?:correct|right|proper|recommended|best) way|(?:correct|right|proper|recommended) way to\b|is this (?:possible|supported)\b|i(?:'d| would) like to know|i want to know|can (?:someone|somebody|anyone)\b|does an(?:yone|ybody) know|help me understand|not sure (?:how|if|whether)\b)`)

// reQuestionBoilerplate rejects sentences that are not the author's own ask:
// the old issue templates' interrogatives (plain prose, so the #-heading
// strip never sees them) and terraform's pasted confirmation prompt.
var reQuestionBoilerplate = regexp.MustCompile(`(?i)(?:open or closed\) or pull requests|should be linked here|vendor (?:blog posts or )?documentation|anything atypical about your account|do you really want to destroy)`)

// reQuestionHowTo is the loosest body signal — "how to" shows up in doc links
// and pasted prose as often as in asks, so it only ever counts as weak.
var reQuestionHowTo = regexp.MustCompile(`(?i)\bhow to\b`)

// questionLead reports whether s opens interrogatively — markdown dressing
// trimmed first so "**Am I missing something?**" still reads as a lead.
func questionLead(s string) bool {
	m := reQuestionLead.FindStringSubmatch(strings.TrimLeft(s, "*_>-— \t"))
	if m == nil {
		return false
	}
	w := strings.ToLower(m[1])
	return w != "not" && w != "no" && w != "n't"
}

// questionTitleEvidence reads the title: a "?" anywhere ("doesn't work?!"
// counts) or a question phrase is strong, a bare interrogative opening is
// weak.
func questionTitleEvidence(title string) *questionEvidence {
	t := strings.TrimSpace(title)
	quote := text.TruncateRunes(text.OneLine(t), 140)
	if strings.Contains(t, "?") || reQuestionPhrase.MatchString(t) {
		return &questionEvidence{quote: quote, source: questionSourceTitle}
	}
	if questionLead(t) {
		return &questionEvidence{quote: quote, source: questionSourceTitle, weak: true}
	}
	return nil
}

// questionSentenceEnds reports whether the byte after a "?" lets it end a
// sentence — URL query strings ("?ref=main") run straight into a word and
// never count.
func questionSentenceEnds(b byte) bool {
	wordish := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '=' || b == '&' || b == '/' || b == '%' || b == '_' || b == '-'
	return !wordish
}

// questionBodyEvidence sweeps the author's prose (code fences, HCL lines, and
// #-headings — the template's own "?" headings included — already stripped)
// for interrogative sentences: each sentence-ending "?" reaches back to the
// previous terminator. An interrogative-lead or phrase-bearing sentence is
// strong; anything else trailing a "?" ("any ideas?") is weak. Question
// phrases outside those sentences ("I was wondering if ...") follow, then a
// lone weak "how to" when nothing else matched — all capped so a chatty body
// cannot flood a finding.
func questionBodyEvidence(prose string) []questionEvidence {
	var out []questionEvidence
	var spans [][2]int
	covered := func(pos int) bool {
		for _, s := range spans {
			if pos >= s[0] && pos < s[1] {
				return true
			}
		}
		return false
	}

	for qi := 0; qi < len(prose) && len(out) < 4; qi++ {
		if prose[qi] != '?' || (qi+1 < len(prose) && !questionSentenceEnds(prose[qi+1])) {
			continue
		}
		start := strings.LastIndexAny(prose[:qi], ".!?\n") + 1
		s := strings.TrimSpace(prose[start : qi+1])
		if len(strings.Fields(s)) < 3 || reQuestionBoilerplate.MatchString(s) {
			continue
		}
		spans = append(spans, [2]int{start, qi + 1})
		out = append(out, questionEvidence{
			quote:  text.TruncateRunes(text.OneLine(s), 140),
			source: questionSourceBody,
			weak:   !questionLead(s) && !reQuestionPhrase.MatchString(s),
		})
	}

	extra := 0
	for _, m := range reQuestionPhrase.FindAllStringIndex(prose, -1) {
		if covered(m[0]) || extra == 2 {
			continue
		}
		extra++
		out = append(out, questionEvidence{
			quote:  text.TruncateRunes(text.OneLine(prose[max(0, m[0]-60):min(len(prose), m[1]+60)]), 140),
			source: questionSourceBody,
		})
	}

	if len(out) == 0 {
		if m := reQuestionHowTo.FindStringIndex(prose); m != nil {
			out = append(out, questionEvidence{
				quote:  text.TruncateRunes(text.OneLine(prose[max(0, m[0]-60):min(len(prose), m[1]+60)]), 140),
				source: questionSourceBody,
				weak:   true,
			})
		}
	}
	return out
}

// Question finds OPEN issues with no question label whose title or body reads
// as an ask — how do I, is it possible, a title ending in "?" — and applies
// the label. Labels are only ever added; the AI reads each issue to tell a
// genuine usage question from a bug report or feature request merely phrased
// as one before anything is applied.
func (f *Flags) Question() error {
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

	findings, open, err := f.collectQuestion(d)
	if err != nil {
		return err
	}
	if open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d open issues read as questions their labels don't record:</>\n", len(findings), open)
	for _, p := range questionKindSummary(findings) {
		cout.Printf("  <gray>%s:</> <yellow>%d</>\n", p.name, p.count)
	}
	if len(findings) == 0 {
		return nil
	}

	switch {
	case f.Modes.ApplyWithAI || f.Modes.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyQuestion(d, findings, true)
	case f.Modes.Apply:
		return f.applyQuestion(d, findings, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.questionJudgeItems(findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passLabelQuestion, promptText, items, nil, nil); err != nil {
			return err
		}
		cli.SortByVerdict(findings, func(x *questionFinding) int { return x.issue.Number }, verdicts)
	} else {
		cout.Printf("<gray>--ai=false: listing without scores</>\n")
	}

	for n := range findings {
		f.printQuestionCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi label question --apply --dry-run</> to preview the labels, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// questionKindPart is one line of the by-kind summary.
type questionKindPart struct {
	name  string
	count int
}

// questionKindSummary counts findings by the kind their labels already claim,
// unkinded first — those are the point, a kind label is what they gain.
func questionKindSummary(findings []questionFinding) []questionKindPart {
	byKind := map[string]int{}
	for i := range findings {
		byKind[findings[i].kind]++
	}
	var parts []questionKindPart
	if byKind[""] > 0 {
		parts = append(parts, questionKindPart{"no kind label", byKind[""]})
	}
	for _, k := range []string{"bug", "crash", "enhancement", "documentation"} {
		if byKind[k] > 0 {
			parts = append(parts, questionKindPart{"labelled " + k, byKind[k]})
		}
	}
	return parts
}

// collectQuestion gathers the evidence: every open issue with no question
// label whose title or body carries a question shape.
func (f *Flags) collectQuestion(d *db.DB) ([]questionFinding, int, error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, 0, err
	}
	cout.Printf("scanning <yellow>%d</> open issues for unlabelled questions...\n", len(issues))

	var findings []questionFinding
	for _, i := range issues {
		if i.HasLabel(labelQuestion) {
			continue
		}
		var evidence []questionEvidence
		if e := questionTitleEvidence(i.Title); e != nil {
			evidence = append(evidence, *e)
		}
		evidence = append(evidence, questionBodyEvidence(issue.Prose(issue.CleanBody(i.Body)))...)
		// a lone weak lead is not worth a finding — an interrogative title or
		// a stray "how to" only corroborates, it never proposes on its own
		strong := false
		for _, e := range evidence {
			strong = strong || !e.weak
		}
		if !strong {
			continue
		}
		findings = append(findings, questionFinding{
			issue:    i,
			kind:     issue.KindFromLabels(i.Labels),
			evidence: evidence,
		})
	}

	slices.SortStableFunc(findings, func(a, b questionFinding) int {
		return a.issue.Number - b.issue.Number
	})
	return findings, len(issues), nil
}

// applyQuestion adds the label on the shared harness: plain --apply trusts
// the quotes; --apply-with-ai[-auto] gates each issue on the judge reading
// them — pipelined, so batch N is reviewed and applied while batch N+1 is
// already off being scored.
func (f *Flags) applyQuestion(d *db.DB, findings []questionFinding, withAI bool) error {
	byNumber := map[int]*questionFinding{}
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

	p := f.NewApplyPass(f.Modes,
		func(n int) string { return byNumber[n].issue.Title },
		func(n int, v *issue.Verdict, pos, total int, interactive bool) (int, error) {
			fdg := byNumber[n]
			f.printQuestionCard(fdg, pos, total, v)
			return f.addLabels(repo, fdg.issue, []string{labelQuestion}, throttle, interactive)
		})
	p.Noun = "issues missing the question label"
	p.GateLabel = "is-question"
	labelVerbs(p)
	p.ConfirmAll = fmt.Sprintf("add the question label to up to <yellow>%d</> issues in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("add the question label to issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.questionJudgeItems(findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passLabelQuestion, promptText, items, onReady, onBatch)
		return jerr
	})
}

// questionJudgeItems renders one judge block per finding: the issue, its
// existing labels, the body, and every quote, so the AI can tell a genuine
// question from a report or request merely phrased as one.
func (f *Flags) questionJudgeItems(findings []questionFinding) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptLabelQuestion)
	if err != nil {
		return "", nil, err
	}

	items := make([]issue.JudgeItem, 0, len(findings))
	for i := range findings {
		fdg := &findings[i]
		var b strings.Builder
		fmt.Fprintf(&b, "### Issue #%d: %s\n", fdg.issue.Number, text.OneLine(fdg.issue.Title))
		fmt.Fprintf(&b, "opened %s, last activity %s\n", fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		if len(fdg.issue.Labels) > 0 {
			fmt.Fprintf(&b, "EXISTING LABELS: %s\n", strings.Join(fdg.issue.Labels, ", "))
		}
		fmt.Fprintf(&b, "ISSUE BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		b.WriteString("QUESTION-SHAPED QUOTES:\n")
		for _, e := range fdg.evidence {
			mark := ""
			if e.weak {
				mark = ", weak"
			}
			fmt.Fprintf(&b, "- [%s%s] %q\n", e.source, mark, e.quote)
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printQuestionCard is one candidate: the issue, the kind its labels already
// claim, and the quotes behind the proposal.
func (f *Flags) printQuestionCard(fdg *questionFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	if fdg.kind != "" {
		// orange = a kind its labels already claim, a relabel not a fill —
		// distinct from the lightMagenta add
		cout.Printf("      <gray>labelled:</> <lightYellow>%s</>\n", fdg.kind)
	}
	cout.Printf("      <gray>add</> <lightMagenta>%s</><gray>:</>\n", labelQuestion)
	for _, e := range fdg.evidence {
		cout.Printf("        <gray>[%s] “</>%s<gray>”</>\n", e.source, e.quote)
	}
	cli.PrintVerdict(v)
}

// questionReportSection builds the question labeller's section: each issue
// with the kind its labels already claim and the quotes behind the proposal.
func (f *Flags) questionReportSection(d *db.DB, o cli.FlagsReport, now time.Time) (cli.ReportSection, error) {
	s := cli.ReportSection{
		Slug:     "label-question",
		Name:     "label question",
		Question: "this issue reads as a question its labels don't record — label it?",
		Description: "Open issues with no question label whose title or body prose reads as an ask — a \"?\" in the title, " +
			"interrogative sentences, ask phrases like \"how do I\" and \"is it possible\" (code blocks, template text, and " +
			"weak leads like a bare interrogative title never propose on their own). The AI tells a genuine usage question " +
			"from a bug report or feature request merely phrased as one. Labels are only ever added.",
		Command: "koi label question --apply / --apply-with-ai / --apply-with-ai-auto",
	}
	findings, open, err := f.collectQuestion(d)
	if err != nil {
		return s, err
	}
	s.Total = len(findings)
	for _, p := range questionKindSummary(findings) {
		s.Classes = append(s.Classes, cli.ReportClass{Name: p.name, Count: p.count, Kind: cli.KindMid})
	}
	s.Note = fmt.Sprintf("%d open issues scanned · labels are add-only, existing labels are never touched", open)

	findings, s.Truncated = cli.LimitFindings(findings, o.Limit)
	var verdicts map[int]*issue.Verdict
	if o.WithAI && len(findings) > 0 {
		promptText, items, jerr := f.questionJudgeItems(findings)
		if jerr != nil {
			return s, jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passLabelQuestion, promptText, items, nil, nil); err != nil {
			return s, err
		}
		cli.SortByVerdict(findings, func(x *questionFinding) int { return x.issue.Number }, verdicts)
	}

	for i := range findings {
		fdg := &findings[i]
		item := cli.ReportItem{
			Number: fdg.issue.Number, URL: fdg.issue.URL, Title: text.OneLine(fdg.issue.Title),
			Meta: fmt.Sprintf("opened %s ago · last activity %s ago · 💬 %d · 👍 %d",
				text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now),
				fdg.issue.CommentCount, fdg.issue.ThumbsUp),
		}
		if fdg.kind != "" {
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("labelled:", cli.KindDim), cli.Span(fdg.kind, cli.KindWarn),
			})
		}
		item.Evidence = append(item.Evidence, []cli.ReportSpan{
			cli.Span("add", cli.KindDim), cli.Span(labelQuestion, cli.KindVer),
		})
		for _, e := range fdg.evidence {
			kind := cli.KindOK
			if e.weak {
				kind = cli.KindWarn
			}
			item.Evidence = append(item.Evidence, []cli.ReportSpan{
				cli.Span("["+e.source+"]", kind),
				cli.Span("“"+e.quote+"”", cli.KindQuote),
			})
		}
		cli.AttachVerdict(&item, verdicts[fdg.issue.Number])
		s.Items = append(s.Items, item)
	}
	return s, nil
}
