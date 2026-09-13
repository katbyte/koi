package issue

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/text"
)

// Card bundles everything the review card needs for one proposal. The
// card's job is to let a human decide in seconds: every load-bearing fact —
// version evidence with its quote, the newest claim, linked PRs, the AI's
// judgement, and the informative comments — is on screen, highlighted.
type Card struct {
	Issue   *db.Issue
	Signals *db.Signals
	Action  *db.Action

	comments []db.Comment
	prs      []db.Crossref  // linked PRs in the triaged repo only — foreign-repo mentions are noise
	releases map[int]string // PR number -> release that shipped it, per the changelog
	mentions []Claim        // every version claim in the thread, with quote + comment url
	now      time.Time

	keepReactions int // 👍 threshold that reddens the count
	currentMajor  int // what "recent claim" is judged against
}

// LoadCard assembles the card for one proposal. repo scopes linked PRs to the
// triaged repository; keepReactions and currentMajor colour the engagement and
// claim lines.
func LoadCard(d *db.DB, a *db.Action, repo string, keepReactions, currentMajor int) (*Card, error) {
	i, err := d.GetIssue(a.IssueNumber)
	if err != nil {
		return nil, err
	}
	if i == nil {
		return nil, fmt.Errorf("issue #%d is in the actions table but not the issues table", a.IssueNumber)
	}

	s, err := d.GetSignals(a.IssueNumber)
	if err != nil {
		return nil, err
	}
	if s == nil {
		s = &db.Signals{IssueNumber: a.IssueNumber}
	}

	comments, err := d.CommentsFor(a.IssueNumber)
	if err != nil {
		return nil, err
	}

	crossrefs, err := d.CrossrefsFor(a.IssueNumber)
	if err != nil {
		return nil, err
	}
	var prs []db.Crossref
	releases := map[int]string{}
	for _, r := range crossrefs {
		if !r.IsPR || !strings.EqualFold(r.RefRepo, repo) {
			continue
		}
		prs = append(prs, r)
		if r.Merged {
			if versions, err := d.ChangelogVersionsForPR(r.RefNumber); err == nil && len(versions) > 0 {
				sort.Slice(versions, func(a, b int) bool { return VersionLess(versions[a], versions[b]) })
				releases[r.RefNumber] = versions[0]
			}
		}
	}

	return &Card{
		Issue: i, Signals: s, Action: a, comments: comments,
		prs: prs, releases: releases, mentions: VersionMentions(comments),
		now: time.Now(), keepReactions: keepReactions, currentMajor: currentMajor,
	}, nil
}

// Render prints the full card.
func (c *Card) Render(pos, total int) {
	i, s, a := c.Issue, c.Signals, c.Action

	cout.Printf("\n<gray>%s</>\n", strings.Repeat("─", 100))
	cout.Printf("<lightBlue>[%d/%d]</> <cyan>#%d</> <bold>%s</> <darkGray>%s</>\n", pos, total, i.Number, text.TruncateRunes(i.Title, 90), i.URL)

	// labels · age · author · engagement
	labels := strings.Join(i.Labels, " · ")
	if labels == "" {
		labels = "no labels"
	}
	cout.Printf("  <gray>%s</>\n", labels)
	cout.Printf("  opened <yellow>%s</> ago by %s (%s) · 💬 <yellow>%d</> · 👍 %s · %d participants · last activity <yellow>%s</> ago%s\n",
		text.HumanAge(i.CreatedAt, c.now), i.Author, strings.ToLower(i.AuthorAssociation),
		i.CommentCount, thumbsColoured(i.ThumbsUp, c.keepReactions),
		s.Participants, text.HumanAge(s.LastActivity, c.now), maintainerTag(s.MaintainerCommented))

	// version evidence
	if s.VersionMajor > 0 {
		cout.Printf("  version: <lightMagenta>v%s</> <gray>(%s)</> — %s\n", VersionText(s), s.VersionSource, text.TruncateRunes(s.VersionQuote, 80))
	} else {
		cout.Printf("  version: <gray>undetermined</>\n")
	}

	// claims: for a close proposal, "no recent claims" is the load-bearing fact
	switch {
	case s.NewestClaimMajor > 0 && s.NewestClaimMajor >= c.currentMajor-1:
		cout.Printf("  claim: <red>v%d.x mentioned %s ago</> by @%s — \"%s\"\n",
			s.NewestClaimMajor, text.HumanAge(s.NewestClaimAt, c.now), s.NewestClaimAuthor, text.TruncateRunes(s.NewestClaimQuote, 80))
	case s.NewestClaimMajor > 0:
		cout.Printf("  claim: newest version mentioned in comments is <lightMagenta>v%d.x</> (%s ago by @%s) — \"%s\"\n",
			s.NewestClaimMajor, text.HumanAge(s.NewestClaimAt, c.now), s.NewestClaimAuthor, text.TruncateRunes(s.NewestClaimQuote, 70))
	default:
		cout.Printf("  claim: <green>no version claims found in comments</>\n")
	}
	c.renderVersionMentions()

	// linked PRs, one per line (same repo only — foreign-repo mentions are noise)
	c.renderLinkedPRs()

	// resources
	if len(s.Resources) > 0 {
		cout.Printf("  <gray>resources: %s</>\n", strings.Join(s.Resources, ", "))
	}

	// comment digest — the gold for old issues
	c.renderCommentDigest()

	// AI verdicts

	// the proposal itself
	cout.Printf("  <gray>%s</>\n", strings.Repeat("─", 60))
	switch a.Action {
	case db.ActionClose:
		cout.Printf("  <red>CLOSE</> as %s · <bold>%s</> · confidence %s · template <cyan>%s.md</>\n",
			a.StateReason, a.Reason, ConfidenceColoured(a.Confidence), a.Template)
	case db.ActionKeep:
		cout.Printf("  <green>KEEP</> · <bold>%s</> · confidence %s\n", a.Reason, ConfidenceColoured(a.Confidence))
	default:
		cout.Printf("  <yellow>NEEDS HUMAN</> · %s\n", a.Reason)
	}
	for _, k := range text.SortedKeys(a.Evidence) {
		cout.Printf("    <gray>%s:</> %s\n", k, text.TruncateRunes(a.Evidence[k], 110))
	}
}

// renderCommentDigest shows the informative slice of the thread: every comment
// with a version mention plus the most recent ones, versions highlighted.
func (c *Card) renderCommentDigest() {
	if len(c.comments) == 0 {
		return
	}
	picked := DigestComments(c.comments, 5)
	cout.Printf("  <gray>── comments (%d of %d — c for all) ──</>\n", len(picked), len(c.comments))
	for _, cm := range picked {
		c.renderCommentLine(&cm, 150)
	}
}

func (c *Card) renderCommentLine(cm *db.Comment, width int) {
	body := text.TruncateRunes(text.OneLine(CleanBody(cm.Body)), width)
	body = HighlightVersions(body, "<lightMagenta>", "</>")
	marker := ""
	if cm.IsMaintainer() {
		marker = " <green>[maintainer]</>"
	}
	if HasVersionMention(cm.Body) {
		marker += " <yellow>[version]</>"
	}
	cout.Printf("   <gray>%5s</> <cyan>@%s</>%s: %s\n", text.HumanAge(cm.CreatedAt, c.now), cm.Author, marker, body)
}

// RenderAllComments prints the full thread (the c key).
func (c *Card) RenderAllComments() {
	cout.Printf("\n<gray>── all %d comments on #%d ──</>\n", len(c.comments), c.Issue.Number)
	for i := range c.comments {
		c.renderCommentLine(&c.comments[i], 400)
	}
}

// RenderBody prints the cleaned issue body (the b key).
func (c *Card) RenderBody() {
	cout.Printf("\n<gray>── body of #%d ──</>\n", c.Issue.Number)
	cout.Println(text.TruncateRunes(CleanBody(c.Issue.Body), 4000))
}

// renderLinkedPRs prints each same-repo linked PR on its own line: state, title,
// and — for merged ones the changelog knows — the release that shipped it.
func (c *Card) renderLinkedPRs() {
	if len(c.prs) == 0 {
		return
	}
	cout.Printf("  linked PRs:\n")
	shown := 0
	for _, r := range c.prs {
		if shown >= 8 {
			cout.Printf("    <gray>… and %d more</>\n", len(c.prs)-shown)
			break
		}
		shown++
		title := text.TruncateRunes(r.Title, 70)
		switch {
		case r.Merged:
			shipped := ""
			if rel, ok := c.releases[r.RefNumber]; ok {
				shipped = fmt.Sprintf(" — shipped in <lightMagenta>%s</>", rel)
			}
			closes := ""
			if r.WillClose {
				closes = " <green>[closes this issue]</>"
			}
			cout.Printf("    <lightMagenta>merged</> <lightCyan>#%d</>%s %q%s\n", r.RefNumber, closes, title, shipped)
		case r.State == db.IssueOpen:
			note := "← unmerged work in flight"
			if r.WillClose {
				note = "← will close this issue when merged"
			}
			cout.Printf("    <yellow>OPEN</>   <lightCyan>#%d</> %q <yellow>%s</>\n", r.RefNumber, title, note)
		default:
			cout.Printf("    <gray>closed</> <lightCyan>#%d</> <gray>%q (never merged)</>\n", r.RefNumber, title)
		}
	}
}

// renderVersionMentions lists every version claim in the thread — one line per
// mention with its quote, plus the comment url so the reviewer can jump straight
// to the evidence. Only shown when there is more than one mention (the single
// newest one is already on the claim line above).
func (c *Card) renderVersionMentions() {
	if len(c.mentions) < 2 {
		return
	}
	cout.Printf("  version mentions in thread:\n")
	shown := 0
	for _, m := range c.mentions {
		if shown >= 8 {
			cout.Printf("    <gray>… and %d more</>\n", len(c.mentions)-shown)
			break
		}
		shown++
		cout.Printf("    <lightMagenta>v%d.x</> <gray>%5s ago</> <cyan>@%s</> — %q\n", m.Major, text.HumanAge(m.At, c.now), m.Author, text.TruncateRunes(m.Quote, 90))
		if m.URL != "" {
			cout.Printf("          <gray>%s</>\n", m.URL)
		}
	}
}

// DigestComments picks the informative slice: every version-mentioning comment
// (newest first, up to 3) plus the last two overall, in chronological order.
func DigestComments(comments []db.Comment, maxN int) []db.Comment {
	if len(comments) <= maxN {
		return comments
	}

	pickedIdx := map[int]bool{}

	// version mentions, newest first
	versioned := 0
	for i := len(comments) - 1; i >= 0 && versioned < 3; i-- {
		if HasVersionMention(comments[i].Body) {
			pickedIdx[i] = true
			versioned++
		}
	}

	// the last two comments (thread state now)
	for i := len(comments) - 1; i >= 0 && len(pickedIdx) < maxN; i-- {
		if len(comments)-i > 2 {
			break
		}
		pickedIdx[i] = true
	}

	// first maintainer response, if any room left
	for i := range comments {
		if len(pickedIdx) >= maxN {
			break
		}
		if comments[i].IsMaintainer() {
			pickedIdx[i] = true
			break
		}
	}

	idxs := make([]int, 0, len(pickedIdx))
	for i := range pickedIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	out := make([]db.Comment, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, comments[i])
	}
	return out
}

// VersionText renders a signals row's version: the full version when known,
// else "N.x" from the major.
func VersionText(s *db.Signals) string {
	if s.VersionFull != "" {
		return s.VersionFull
	}
	return strconv.Itoa(s.VersionMajor) + ".x"
}

func maintainerTag(commented bool) string {
	if commented {
		return " · <green>maintainer replied</>"
	}
	return ""
}

// ConfidenceColoured buckets a confidence for display: green ≥0.75, yellow ≥0.5,
// orange ≥0.25, red below.
func ConfidenceColoured(c float64) string {
	switch {
	case c >= 0.75:
		return fmt.Sprintf("<green>%.2f</>", c)
	case c >= 0.5:
		return fmt.Sprintf("<yellow>%.2f</>", c)
	case c >= 0.25:
		return fmt.Sprintf("<fg=208>%.2f</>", c)
	default:
		return fmt.Sprintf("<red>%.2f</>", c)
	}
}

// thumbsColoured renders the 👍 count, red when at/over the keep threshold.
func thumbsColoured(n, threshold int) string {
	if n >= threshold {
		return fmt.Sprintf("<red>%d</>", n)
	}
	return strconv.Itoa(n)
}
