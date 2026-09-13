package issue

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
)

// AnalyseAll computes signals + rule proposals for every open issue, printing a
// single summary line. Returns nil counts when the db has no open issues (a
// message is printed either way). Deterministic and free: safe to re-run any
// time, and it never overwrites decisions a human has already made.
func AnalyseAll(d *db.DB, repo string, cfg RuleConfig) (map[string]int, error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		cout.Printf("no open issues in db — run <cyan>koi fetch</> first\n")
		return nil, nil
	}

	counts := map[string]int{}
	for _, i := range issues {
		s, err := computeSignals(d, i, repo)
		if err != nil {
			return nil, err
		}

		// earlier AI runs enriched signals the deterministic parse can't derive;
		// a re-run must not wipe that enrichment (the verdict cache means it
		// wouldn't re-derive it for an unchanged issue)
		prev, err := d.GetSignals(i.Number)
		if err != nil {
			return nil, err
		}
		if prev != nil && prev.VersionSource == "ai" && s.VersionMajor == 0 {
			s.VersionMajor, s.VersionFull, s.VersionSource, s.VersionQuote = prev.VersionMajor, prev.VersionFull, prev.VersionSource, prev.VersionQuote
		}
		if prev != nil && s.Kind == "" && prev.Kind != "" {
			s.Kind = prev.Kind // labels gave nothing, so the previous kind was AI's
		}

		// re-analysing an unchanged issue must not cost a write: ComputedAt
		// alone moving would dirty every row on every run
		if prev == nil || !signalsEqual(prev, s) {
			if err := d.SaveSignals(s); err != nil {
				return nil, err
			}
		}

		if a := Propose(i, s, cfg); a != nil {
			// proposals refined by the AI passes or edited by a human outlive an
			// analyse re-run: rules alone can't reproduce an ai-keep veto, and the
			// verdict cache means an AI run wouldn't re-derive it either
			existing, err := d.GetAction(i.Number)
			if err != nil {
				return nil, err
			}
			if existing != nil && existing.Status == db.StatusProposed && existing.Source != "rules" {
				counts[existing.Action+"/"+existing.Reason]++
			} else {
				if _, err := d.ProposeAction(a); err != nil {
					return nil, err
				}
				counts[a.Action+"/"+a.Reason]++
			}
		}
	}

	closes := 0
	for k, n := range counts {
		if strings.HasPrefix(k, db.ActionClose+"/") {
			closes += n
		}
	}
	cout.Printf("<gray>analysed %d open issues — %d close proposals up to date</>\n", len(issues), closes)
	return counts, nil
}

// signalsEqual reports whether two signal rows carry the same facts, ignoring
// ComputedAt. Keep in sync with db.Signals — a field missed here means its
// changes stop being persisted.
func signalsEqual(a, b *db.Signals) bool {
	return a.IssueNumber == b.IssueNumber && a.Kind == b.Kind &&
		a.VersionMajor == b.VersionMajor && a.VersionFull == b.VersionFull &&
		a.VersionSource == b.VersionSource && a.VersionQuote == b.VersionQuote &&
		slices.Equal(a.Resources, b.Resources) && a.Service == b.Service &&
		a.LastActivity.Equal(b.LastActivity) && a.MaintainerCommented == b.MaintainerCommented &&
		a.Participants == b.Participants &&
		a.NewestClaimMajor == b.NewestClaimMajor && a.NewestClaimAt.Equal(b.NewestClaimAt) &&
		a.NewestClaimQuote == b.NewestClaimQuote && a.NewestClaimAuthor == b.NewestClaimAuthor &&
		a.OpenLinkedPRs == b.OpenLinkedPRs && a.MergedLinkedPRs == b.MergedLinkedPRs &&
		a.MergedPRNumber == b.MergedPRNumber && a.MergedPRTitle == b.MergedPRTitle &&
		a.MultiVersionLabels == b.MultiVersionLabels
}

// computeSignals derives everything the rules and the review card need for one issue.
func computeSignals(d *db.DB, i *db.Issue, repo string) (*db.Signals, error) {
	comments, err := d.CommentsFor(i.Number)
	if err != nil {
		return nil, err
	}
	crossrefs, err := d.CrossrefsFor(i.Number)
	if err != nil {
		return nil, err
	}

	s := &db.Signals{
		IssueNumber: i.Number,
		Kind:        KindFromLabels(i.Labels),
		Service:     ServiceFromLabels(i.Labels),
		Resources:   ExtractResources(i.Title+"\n"+i.Body, 10),
		ComputedAt:  db.Now(),
	}

	// version precedence: maintainer label > template block > body mentions
	if major, count := VersionFromLabels(i.Labels); major > 0 {
		s.VersionMajor, s.VersionFull, s.VersionSource = major, "", "label"
		s.VersionQuote = "labelled v/" + strconv.Itoa(major) + ".x"
		s.MultiVersionLabels = count > 1
	} else if v := ExtractProviderVersion(i.Body); v != nil {
		s.VersionMajor, s.VersionFull, s.VersionSource, s.VersionQuote = v.Major, v.Full, v.Source, v.Quote
	}

	// last *human* activity: the newest of issue creation and comments — label
	// churn bumps updated_at but shouldn't keep an issue "active"
	s.LastActivity = i.CreatedAt
	participants := map[string]bool{i.Author: true}
	for i := range comments {
		c := &comments[i]
		if c.CreatedAt.After(s.LastActivity) {
			s.LastActivity = c.CreatedAt
		}
		if c.IsMaintainer() {
			s.MaintainerCommented = true
		}
		participants[c.Author] = true
	}
	s.Participants = len(participants)

	if claim := SweepClaims(comments); claim != nil {
		s.NewestClaimMajor = claim.Major
		s.NewestClaimAt = claim.At
		s.NewestClaimQuote = claim.Quote
		s.NewestClaimAuthor = claim.Author
	}

	var newestMerge time.Time
	for _, r := range crossrefs {
		// same-repo PRs only: anything on GitHub that mentions the issue number —
		// including random personal forks — lands in the timeline, and a PR in
		// someone else's repo can't have fixed or be fixing anything here
		if !r.IsPR || !strings.EqualFold(r.RefRepo, repo) {
			continue
		}
		switch {
		case r.State == db.IssueOpen:
			s.OpenLinkedPRs++
		case r.Merged:
			s.MergedLinkedPRs++
			if r.MergedAt.After(newestMerge) {
				newestMerge = r.MergedAt
				s.MergedPRNumber, s.MergedPRTitle = r.RefNumber, r.Title
			}
		}
	}

	return s, nil
}
