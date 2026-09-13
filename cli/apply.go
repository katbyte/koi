// The shared apply harness pieces: throttle, guards, and the flag-fed
// ApplyPass constructor.

package cli

import (
	"fmt"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// NewApplyPass wires the flag-level knobs shared by every check into the
// harness (lib/issue's ApplyPass); the caller fills the per-pass wording
// (Noun, GateLabel, ConfirmAll, ConfirmAI).
func (f *FlagData) NewApplyPass(m FlagsApplyModes, title func(int) string, closeOne issue.CloseFunc) *issue.ApplyPass {
	threshold := m.Threshold
	if threshold <= 0 {
		threshold = JudgeThreshold
	}
	return &issue.ApplyPass{
		RepoTag:   f.RepoTag(),
		DryRun:    f.DryRun,
		Yes:       f.Yes,
		Auto:      m.ApplyWithAIAuto,
		Max:       m.Max,
		Threshold: threshold,
		Title:     title,
		URL:       f.IssueURL,
		ScoreTag:  ScoreTag,
		Close:     closeOne,
	}
}

// Apply executes approved close actions: comment from the template, then close
// with the right state_reason. Before every mutation the issue is re-fetched and
// skipped as stale if it changed since the decision — new activity means a human
// re-look, not a robo-close.
func (f *FlagData) Apply() error {
	reason, maxApply := f.Cmd.ApplyReason, f.Modes.Max
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}

	actions, err := d.Actions(db.ActionFilter{Status: db.StatusApproved, Action: db.ActionClose, Reason: reason, Limit: maxApply})
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		cout.Printf("no approved close actions to apply — run <cyan>koi review</> first\n")
		return nil
	}

	counts := map[string]int{}
	for _, a := range actions {
		counts["close/"+a.Reason]++
	}
	cout.Printf("applying <yellow>%d</> closes to <white>%s</>%s:\n", len(actions), f.GH.Repo, issue.DryRunTag(f.DryRun))
	text.PrintCounts(counts)

	if !f.DryRun && !f.Yes {
		ok, err := issue.Confirm(fmt.Sprintf("close <yellow>%d</> issues on %s?", len(actions), f.RepoTag()))
		if err != nil {
			return err
		}
		if !ok {
			cout.Printf("aborted\n")
			return nil
		}
	}

	throttle := NewThrottle()
	applied, stale, failed := 0, 0, 0

	for n, a := range actions {
		card, err := issue.LoadCard(d, a, f.GH.Repo, f.KeepReactions, f.CurrentMajor)
		if err != nil {
			return err
		}

		cout.Printf("  <gray>%d/%d</> <cyan>#%d</> <bold>%s</> <darkGray>%s</>\n",
			n+1, len(actions), a.IssueNumber, text.TruncateRunes(text.OneLine(card.Issue.Title), 90), f.IssueURL(a.IssueNumber))
		version := "no version"
		if card.Signals != nil && card.Signals.VersionMajor > 0 {
			version = fmt.Sprintf("<lightMagenta>v%s</> <gray>(%s)</>", issue.VersionText(card.Signals), card.Signals.VersionSource)
		}
		cout.Printf("      <lightBlue>%s</> · %s · confidence %s · 💬 %d · 👍 %d\n",
			a.Reason, version, issue.ConfidenceColoured(a.Confidence), card.Issue.CommentCount, card.Issue.ThumbsUp)

		comment, err := issue.RenderCloseComment(card.Issue, card.Signals, a, f.CurrentMajor)
		if err != nil {
			cout.Errorf("      <red>rendering comment: %v</>\n", err)
			if err := d.MarkApplied(a.ID, db.StatusFailed, err.Error()); err != nil {
				return err
			}
			failed++
			continue
		}

		if f.DryRun {
			cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
				len(comment), a.Template, a.StateReason)
			continue
		}

		// staleness guard: skip anything that moved since the decision was made
		throttle()
		live, err := repo.GetIssue(a.IssueNumber)
		if err != nil {
			cout.Errorf("      <red>fetching live state: %v</>\n", err)
			if err := d.MarkApplied(a.ID, db.StatusFailed, err.Error()); err != nil {
				return err
			}
			failed++
			continue
		}
		if live.State != RESTStateOpen {
			cout.Printf("      <gray>already closed on github — skipped</>\n")
			if err := d.MarkApplied(a.ID, db.StatusStale, "already closed"); err != nil {
				return err
			}
			stale++
			continue
		}
		// second-precision slack: db timestamps are truncated to the second
		if live.UpdatedAt.After(a.IssueUpdatedAt.Add(2 * time.Second)) {
			cout.Printf("      <fg=208>changed since the decision (%s > %s) — marked stale for re-review</>\n",
				live.UpdatedAt.Format(time.RFC3339), a.IssueUpdatedAt.Format(time.RFC3339))
			if err := d.MarkApplied(a.ID, db.StatusStale, "issue updated since decision"); err != nil {
				return err
			}
			stale++
			continue
		}

		throttle()
		if err := repo.CreateComment(a.IssueNumber, comment); err != nil {
			cout.Errorf("      <red>comment failed: %v</>\n", err)
			if err := d.MarkApplied(a.ID, db.StatusFailed, err.Error()); err != nil {
				return err
			}
			failed++
			continue
		}

		throttle()
		if err := repo.CloseIssue(a.IssueNumber, a.StateReason); err != nil {
			cout.Errorf("      <red>close failed (comment was posted): %v</>\n", err)
			if err := d.MarkApplied(a.ID, db.StatusFailed, "commented but close failed: "+err.Error()); err != nil {
				return err
			}
			failed++
			continue
		}

		if err := d.MarkApplied(a.ID, db.StatusApplied, ""); err != nil {
			return err
		}
		applied++
		cout.Printf("      <fg=28>commented + closed as</> <lightMagenta>%s</>\n", a.StateReason)
		cout.QuietOnlyf("%d@closed@%s\n", a.IssueNumber, a.Reason)
	}

	if f.DryRun {
		cout.Printf("\n<yellow>dry-run:</> %d closes previewed, nothing changed\n", len(actions))
		return nil
	}

	cout.Printf("\n<green>%d closed</> · %d stale (re-review) · %d failed\n", applied, stale, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d closes failed", failed, len(actions))
	}
	return nil
}

// Reopen reopens an issue (mistake recovery) and records it on the action row.
func (f *FlagData) Reopen(number int) error {
	comment := f.Cmd.ReopenComment
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	repo, err := f.NewRepo()
	if err != nil {
		return err
	}

	if f.DryRun {
		cout.Printf("<yellow>dry-run: would reopen</> <cyan>#%d</> <darkGray>%s</>\n", number, f.IssueURL(number))
		return nil
	}

	if comment != "" {
		if err := repo.CreateComment(number, comment); err != nil {
			return err
		}
	}
	if err := repo.ReopenIssue(number); err != nil {
		return err
	}

	if a, err := d.GetAction(number); err != nil {
		return err
	} else if a != nil {
		if err := d.MarkApplied(a.ID, db.StatusReopened, ""); err != nil {
			return err
		}
	}

	cout.Printf("<fg=28>reopened</> <cyan>#%d</> <darkGray>%s</>\n", number, f.IssueURL(number))
	return nil
}

// mutationThrottle keeps ~2s between GitHub mutations: friendly to secondary
// rate limits, and a runaway apply can be ^C'd before much damage.
const mutationThrottle = 2100 * time.Millisecond

// RESTStateOpen is the REST API's lowercase issue/PR state.
const RESTStateOpen = "open"

// NewThrottle returns a func that sleeps to keep at least mutationThrottle
// between calls (no sleep on the first call).
func NewThrottle() func() {
	var lastCall time.Time
	return func() {
		if !lastCall.IsZero() {
			if wait := mutationThrottle - time.Since(lastCall); wait > 0 {
				time.Sleep(wait)
			}
		}
		lastCall = time.Now()
	}
}

// RejectedInReview reports whether a human already rejected an action for
// this issue in koi review. The checks re-derive candidates from evidence on
// every run, so without this guard a rejected candidate would be re-proposed
// and closed on the next apply — with review promising "won't be proposed
// again".
func RejectedInReview(d *db.DB, number int) (bool, error) {
	a, err := d.GetAction(number)
	if err != nil {
		return false, err
	}
	return a != nil && a.Status == db.StatusRejected, nil
}
