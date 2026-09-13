package close

import (
	"errors"
	"fmt"
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

// Classes by what the thread holds, strongest first: answered has positive
// evidence (a reply that looks like the answer), dead has only silence.
const (
	passQuestions          = "questions"
	promptQuestions        = "issue-question-close"
	templateQuestionsClose = "questions-close"
	reasonQuestionAnswered = "question-answered"
	// the dead class reuses issue.ReasonStaleQuestion — same close, and the
	// old rules-path proposals it supersedes carry that reason already

	classQAnswered = "answered"
	classQDead     = "dead"

	// the signal kind this check owns (bug/crash live in legacy.go)
	signalKindQuestion = "question"

	// an answered thread must have settled (the asker had time to dispute the
	// answer) and a dead one must be well past hope before either closes
	questionsQuietDays = 90
	questionsDeadDays  = 365

	// a reply this short ("+1", "any update?") is not a candidate answer
	questionsAnswerMinRunes = 30
)

var questionsClassRank = map[string]int{classQAnswered: 1, classQDead: 0}

// QuestionsOpts configures the questions audit and its apply modes.
type QuestionsOpts struct {
	Link                string // answered | dead ("" = both)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// questionsFinding is one open question whose thread says it is done with:
// answered carries the reply that looks like the answer (the AI judges whether
// it actually resolves the ask), dead has no substantive reply at all.
type questionsFinding struct {
	issue   *db.Issue
	answer  *db.Comment // best candidate answer (nil for the dead class)
	replies int         // substantive non-asker replies in the thread
	class   string
}

// questionsBot reports whether a comment author is automation, whose comments
// (relabel notices, lock warnings) never answer anything.
func questionsBot(author string) bool {
	return strings.HasSuffix(author, "[bot]") || author == "github-actions" || author == "hashibot"
}

// Questions finds OPEN question-labelled issues that look done with: someone
// answered and the thread settled, or nobody ever replied and it has been dead
// for over a year. The AI reads each thread — does the candidate answer really
// resolve the ask, did the asker push back after it, is this a bug report
// wearing a question label — before blessing a close. Answered closes as
// completed citing the answer; dead closes as not planned.
func (f *Flags) Questions(link string) error {
	o := QuestionsOpts{Link: link, FlagsApplyModes: f.Modes}
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

	col, err := f.collectQuestions(d, o.Link)
	if err != nil {
		return err
	}
	if col.open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}
	findings := col.findings

	cout.Printf("\n<bold>%d of %d open questions look done with:</>\n", len(findings), col.questions)
	for _, c := range []struct{ class, tag, desc string }{
		{classQAnswered, cli.TagGreen, "a reply looks like the answer and the thread settled"},
		{classQDead, cli.TagOrange, "no substantive reply, quiet for over a year"},
	} {
		if n := col.counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-9s</> <yellow>%d</>  <gray>%s</>\n", c.tag, c.class, n, c.desc)
		}
	}
	cout.Printf("  <gray>skipped: %d with recent activity · %s</>\n", col.active, keepSummary(col.protected))
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyQuestions(d, findings, o, true)
	case o.Apply:
		return f.applyQuestions(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.questionsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passQuestions, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b questionsFinding) int {
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
		f.printQuestionsCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close questions --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// questionsCollection is everything collectQuestions learns in one scan.
type questionsCollection struct {
	findings  []questionsFinding
	counts    map[string]int
	open      int            // open issues in the db
	questions int            // open question-labelled issues
	active    int            // recent activity — the conversation may be live
	protected map[string]int // keep guards by reason
}

// collectQuestions walks the open question-labelled issues and classes each
// thread: answered (a substantive non-asker reply exists — the newest one,
// maintainers preferred, is the candidate answer) once the thread has been
// quiet long enough for the asker to have disputed it, or dead (no substantive
// reply and over a year of silence). Anything with recent activity is a live
// conversation and stays out.
func (f *Flags) collectQuestions(d *db.DB, link string) (*questionsCollection, error) {
	col := &questionsCollection{counts: map[string]int{}, protected: map[string]int{}}
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	col.open = len(issues)
	now := time.Now()

	cout.Printf("scanning open question-labelled issues for answered and dead threads...\n")
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, serr
		}
		if s == nil || s.Kind != signalKindQuestion {
			continue
		}
		col.questions++

		switch {
		case s.OpenLinkedPRs > 0:
			col.protected["open-pr"]++
			continue
		case i.ThumbsUp >= f.KeepReactions:
			col.protected["high-engagement"]++
			continue
		}

		comments, cerr := d.CommentsFor(i.Number)
		if cerr != nil {
			return nil, cerr
		}
		fdg := questionsFinding{issue: i}
		var lastAnswer, lastMaintainer *db.Comment
		for ci := range comments {
			c := &comments[ci]
			if c.Author == i.Author || questionsBot(c.Author) ||
				len(strings.TrimSpace(c.Body)) < questionsAnswerMinRunes {
				continue
			}
			fdg.replies++
			lastAnswer = c
			if c.IsMaintainer() {
				lastMaintainer = c
			}
		}
		if lastMaintainer != nil {
			fdg.answer = lastMaintainer
		} else {
			fdg.answer = lastAnswer
		}

		quiet := now.Sub(i.UpdatedAt)
		switch {
		case fdg.answer != nil && quiet >= questionsQuietDays*24*time.Hour:
			fdg.class = classQAnswered
		case fdg.answer == nil && quiet >= questionsDeadDays*24*time.Hour:
			fdg.class = classQDead
		default:
			col.active++
			continue
		}
		if link != "" && fdg.class != link {
			continue
		}
		col.findings = append(col.findings, fdg)
		col.counts[fdg.class]++
	}

	slices.SortStableFunc(col.findings, func(a, b questionsFinding) int {
		if d := questionsClassRank[b.class] - questionsClassRank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return col, nil
}

// applyQuestions is both apply modes on the shared harness: plain --apply
// closes everything listed; --apply-with-ai[-auto] gates each close on the
// judge and is the recommended path (the candidate answer is only a guess
// until the AI reads the thread).
func (f *Flags) applyQuestions(d *db.DB, findings []questionsFinding, o QuestionsOpts, withAI bool) error {
	byNumber := map[int]*questionsFinding{}
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
			return f.closeOneQuestions(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "questions that look done with"
	p.GateLabel = "closeable"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> questions in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close questions the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.questionsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passQuestions, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneQuestions handles one candidate: card, the questions-close comment
// (citing the answer for the answered class), and the close — completed when
// answered, not planned when dead.
func (f *Flags) closeOneQuestions(d *db.DB, repo gh.Repo, fdg *questionsFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printQuestionsCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	reason, stateReason, what := issue.ReasonStaleQuestion, issue.StateNotPlanned, "stale"
	if fdg.class == classQAnswered {
		reason, stateReason, what = reasonQuestionAnswered, issue.StateCompleted, "answered"
	}

	comment, err := f.renderQuestionsComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateQuestionsClose, stateReason)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as %s?", fdg.issue.Number, what), comment, fdg.issue.URL)
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
	if err := repo.CloseIssue(fdg.issue.Number, stateReason); err != nil {
		cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
		return issue.ApplyFailed, nil
	}

	cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", stateReason)
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reason)

	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reason,
		StateReason: stateReason, Template: templateQuestionsClose,
		Evidence:       map[string]string{evidenceKeyClass: fdg.class},
		Source:         passQuestions,
		IssueUpdatedAt: fdg.issue.UpdatedAt,
	}
	if fdg.answer != nil {
		a.Evidence["answer"] = text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.answer.Body)), 120)
		a.Evidence["author"] = fdg.answer.Author
		a.Evidence["comment-url"] = fdg.answer.URL
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

// renderQuestionsComment renders the close comment: answered cites the answer
// with a deep link, dead gets the gentle staleness wording.
func (f *Flags) renderQuestionsComment(fdg *questionsFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateQuestionsClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateQuestionsClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateQuestionsClose, err)
	}
	data := struct {
		Answered     bool
		Author       string
		URL          string
		CurrentMajor int
	}{Answered: fdg.class == classQAnswered, CurrentMajor: f.CurrentMajor}
	if fdg.answer != nil {
		data.Author, data.URL = fdg.answer.Author, fdg.answer.URL
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateQuestionsClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// questionsJudgeItems renders one judge block per finding: the question, its
// class, the candidate answer with the author's standing, and the thread
// digest so the AI can spot pushback after the answer or a bug in disguise.
func (f *Flags) questionsJudgeItems(d *db.DB, findings []questionsFinding) (string, []issue.JudgeItem, error) {
	promptText, err := f.PreparePrompt(promptQuestions)
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
		fmt.Fprintf(&b, "asked by @%s, opened %s, last activity %s\n",
			fdg.issue.Author, fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "CLASS: %s\n", strings.ToUpper(fdg.class))
		fmt.Fprintf(&b, "QUESTION BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		if fdg.answer != nil {
			fmt.Fprintf(&b, "CANDIDATE ANSWER by %s (%s) on %s:\n%s\n",
				fdg.answer.Author, fdg.answer.AuthorAssociation, fdg.answer.CreatedAt.Format("2006-01-02"),
				text.TruncateRunes(issue.CleanBody(fdg.answer.Body), 1500))
		} else {
			b.WriteString("NO CANDIDATE ANSWER: nobody substantively replied.\n")
		}
		if picked := issue.DigestComments(comments, 10); len(picked) > 0 {
			fmt.Fprintf(&b, "THREAD (%d of %d comments — watch for pushback AFTER the candidate answer):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printQuestionsCard is one candidate: the question, its thread's state, the
// candidate answer with the author's standing, and the AI's score when judged.
func (f *Flags) printQuestionsCard(fdg *questionsFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	now := time.Now()
	cout.Printf("      <gray>asked by</> @%s <gray>%s ago · quiet for %s · %d replies</>\n",
		fdg.issue.Author, text.HumanAge(fdg.issue.CreatedAt, now), text.HumanAge(fdg.issue.UpdatedAt, now), fdg.replies)
	if fdg.answer != nil {
		assocTag, assocName := assocDisplay(fdg.answer.AuthorAssociation)
		who := fmt.Sprintf("<%s>%s</>", assocTag, fdg.answer.Author)
		if assocName != "" {
			who += fmt.Sprintf(" <gray>(%s)</>", assocName)
		}
		cout.Printf("      <gray>candidate answer:</> %s <gray>%s ·</> <gray>“</>%s<gray>”</> <darkGray>%s</>\n",
			who, fdg.answer.CreatedAt.Format("2006-01-02"),
			text.TruncateRunes(text.OneLine(issue.CleanBody(fdg.answer.Body)), 120), fdg.answer.URL)
	} else {
		cout.Printf("      <fg=208>no substantive replies at all</>\n")
	}
	cli.PrintVerdict(v)
}
