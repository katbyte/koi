package close

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// Classes by what the issue leans on, strongest first: a removed resource is
// gone outright, a removed property nearly so; deprecations are still working
// but on notice.
const (
	passDeprecated          = "deprecated"
	promptDeprecated        = "issue-deprecated-close"
	templateDeprecatedClose = "deprecated-close"
	reasonDeprecated        = "deprecated-removed"

	classRemovedResource    = "removed-resource"
	classRemovedProperty    = "removed-property"
	classDeprecatedResource = "deprecated-resource"
	classDeprecatedProperty = "deprecated-property"

	// property tokens word-matching more than this share of open issues are
	// too generic to trust (resource_group, name...) and are skipped
	deprecatedDFCap = 0.03
)

// DeprecatedOpts configures the deprecated audit and its apply modes.
type DeprecatedOpts struct {
	Link                string // resource | property ("" = both types)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// deprecatedMatch is one removal the issue leans on, with the issue line that
// matched for property-level hits.
type deprecatedMatch struct {
	removal db.Removal
	quote   string
}

// deprecatedFinding is one open issue referring to removed/deprecated things.
// matches are sorted strongest first; the close comment cites the first. alive
// holds the issue's other resources — the ones with no resource-level removal
// — so the whole picture is visible: an issue that equally concerns a living
// resource is probably not moot.
type deprecatedFinding struct {
	issue   *db.Issue
	matches []deprecatedMatch
	alive   []string
	class   string
}

var deprecatedClassRank = map[string]int{
	classRemovedResource: 3, classRemovedProperty: 2, classDeprecatedResource: 1, classDeprecatedProperty: 0,
}

// deprecatedTypeOf maps a class to its subcommand scope: resource or property.
func deprecatedTypeOf(class string) string {
	if class == classRemovedProperty || class == classDeprecatedProperty {
		return db.RemovalKindProperty
	}
	return db.RemovalKindResource
}

func deprecatedClassOf(r db.Removal) string {
	prop := r.Kind == db.RemovalKindProperty
	switch {
	case !prop && r.Action == db.RemovalRemoved:
		return classRemovedResource
	case prop && r.Action == db.RemovalRemoved:
		return classRemovedProperty
	case !prop:
		return classDeprecatedResource
	default:
		return classDeprecatedProperty
	}
}

// Deprecated scans every OPEN issue against the removals inventory (upgrade
// guides + changelog DEPRECATIONS): issues asking about or reporting against
// resources, data sources, or properties that no longer exist — or are on the
// way out — where the ask is moot as filed. The AI judges whether each issue's
// substance actually centres on the dead thing; the apply modes close as not
// planned with a comment pointing at the successor.
func (f *Flags) Deprecated(link string) error {
	o := DeprecatedOpts{Link: link, FlagsApplyModes: f.Modes}
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

	findings, counts, noisy, open, err := f.collectDeprecated(d, o.Link)
	if err != nil {
		return err
	}
	if open == 0 {
		cout.Printf("no fetched issues — run <cyan>koi fetch</> first\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d open issues lean on something removed or deprecated:</>\n", len(findings), open)
	for _, c := range []struct{ class, tag string }{
		{classRemovedResource, cli.TagRed},
		{classRemovedProperty, cli.TagOrange},
		{classDeprecatedResource, cli.TagYellow},
		{classDeprecatedProperty, cli.TagGray},
	} {
		if n := counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-20s</> <yellow>%d</>\n", c.tag, c.class, n)
		}
	}
	if len(noisy) > 0 {
		cout.Printf("  <gray>skipped %d too-generic property tokens: %s</>\n", len(noisy), text.TruncateRunes(strings.Join(noisy, " "), 100))
	}
	if len(findings) == 0 {
		return nil
	}

	switch {
	case o.ApplyWithAI || o.ApplyWithAIAuto:
		if !f.AI.Enabled {
			return errors.New("--apply-with-ai needs the AI (--ai=false is set)")
		}
		return f.applyDeprecated(d, findings, o, true)
	case o.Apply:
		return f.applyDeprecated(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest-moot first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.deprecatedJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passDeprecated, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b deprecatedFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without moot scores</>\n")
	}

	for n := range findings {
		f.printDeprecatedCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close deprecated --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// collectDeprecated builds the findings: every open issue whose signals name a
// removed/deprecated resource, or whose text word-matches a removed/deprecated
// property of one of its resources. Returns findings (matches sorted strongest
// first), per-class counts, the skipped too-generic tokens, and the open total.
func (f *Flags) collectDeprecated(d *db.DB, link string) (findings []deprecatedFinding, counts map[string]int, noisy []string, open int, err error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, nil, nil, 0, err
	}
	removals, err := d.Removals()
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if len(removals) == 0 {
		cout.Printf("<yellow>no removals inventory — run koi fetch to parse the upgrade guides</>\n")
		return nil, map[string]int{}, nil, len(issues), nil
	}

	// prefer removed over deprecated when both exist for the same item
	byRank := func(action string) int {
		if action == db.RemovalRemoved {
			return 1
		}
		return 0
	}
	strongest := map[string]db.Removal{}
	for _, r := range removals {
		key := r.Kind + "|" + r.Resource + "|" + r.Property
		if cur, ok := strongest[key]; !ok || byRank(r.Action) > byRank(cur.Action) {
			strongest[key] = r
		}
	}
	resourceLevel := map[string][]db.Removal{}
	propertyLevel := map[string][]db.Removal{}
	for _, r := range strongest {
		if r.Kind == db.RemovalKindProperty {
			propertyLevel[r.Resource] = append(propertyLevel[r.Resource], r)
		} else {
			resourceLevel[r.Resource] = append(resourceLevel[r.Resource], r)
		}
	}

	// property matching: leaf token of dotted paths, matched against each
	// issue's snake-token set (tokenised once per issue — running hundreds of
	// word-boundary regexes over every body took ages), with a document-
	// frequency cap so generic tokens can't flood the scan
	leafOf := func(p string) string {
		parts := strings.Split(p, ".")
		return parts[len(parts)-1]
	}
	matchers := map[string]*regexp.Regexp{}
	for _, props := range propertyLevel {
		for _, r := range props {
			leaf := leafOf(r.Property)
			if _, ok := matchers[leaf]; !ok {
				// the regex only runs on actual hits, to pull the quote line
				matchers[leaf] = regexp.MustCompile(`\b` + regexp.QuoteMeta(leaf) + `\b`)
			}
		}
	}
	cout.Printf("scanning <yellow>%d</> open issues against <yellow>%d</> removals (<yellow>%d</> property tokens)...\n",
		len(issues), len(removals), len(matchers))
	reToken := regexp.MustCompile(`\b[a-z][a-z0-9_]*\b`)
	texts := make(map[int]string, len(issues))
	tokens := make(map[int]map[string]bool, len(issues))
	for _, i := range issues {
		// properties match against PROSE only — a removed property sitting in
		// somebody's pasted config says nothing about what the issue is about,
		// which an -with-ai session over the property class proved emphatically
		t := i.Title + "\n" + issue.Prose(i.Body)
		texts[i.Number] = t
		set := map[string]bool{}
		for _, tok := range reToken.FindAllString(t, -1) {
			set[tok] = true
		}
		tokens[i.Number] = set
	}
	df := map[string]int{}
	for leaf := range matchers {
		for _, set := range tokens {
			if set[leaf] {
				df[leaf]++
			}
		}
	}
	tooGeneric := map[string]bool{}
	for leaf, n := range df {
		if float64(n) > float64(len(issues))*deprecatedDFCap {
			tooGeneric[leaf] = true
		}
	}

	counts = map[string]int{}
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, nil, nil, 0, serr
		}
		if s == nil || len(s.Resources) == 0 {
			continue
		}
		fdg := deprecatedFinding{issue: i}
		for _, res := range s.Resources {
			if len(resourceLevel[res]) == 0 && !slices.Contains(fdg.alive, res) {
				fdg.alive = append(fdg.alive, res)
			}
			for _, r := range resourceLevel[res] {
				fdg.matches = append(fdg.matches, deprecatedMatch{removal: r})
			}
			for _, r := range propertyLevel[res] {
				leaf := leafOf(r.Property)
				if tooGeneric[leaf] || !tokens[i.Number][leaf] {
					continue
				}
				if quote := matchLine(texts[i.Number], matchers[leaf]); quote != "" {
					fdg.matches = append(fdg.matches, deprecatedMatch{removal: r, quote: quote})
				}
			}
		}
		if len(fdg.matches) == 0 {
			continue
		}
		for _, m := range fdg.matches {
			if c := deprecatedClassOf(m.removal); fdg.class == "" || deprecatedClassRank[c] > deprecatedClassRank[fdg.class] {
				fdg.class = c
			}
		}
		// strongest evidence first — the close comment cites matches[0]
		slices.SortStableFunc(fdg.matches, func(a, b deprecatedMatch) int {
			return deprecatedClassRank[deprecatedClassOf(b.removal)] - deprecatedClassRank[deprecatedClassOf(a.removal)]
		})
		if link != "" && deprecatedTypeOf(fdg.class) != link {
			continue
		}
		findings = append(findings, fdg)
		counts[fdg.class]++
	}

	slices.SortStableFunc(findings, func(a, b deprecatedFinding) int {
		if d := deprecatedClassRank[b.class] - deprecatedClassRank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return findings, counts, text.SortedKeys(tooGeneric), len(issues), nil
}

// applyDeprecated is both apply modes on the shared harness: plain --apply
// closes everything listed (the raw evidence includes incidental mentions, so
// it exists for pattern consistency); --apply-with-ai[-auto] gates each close
// on the judge and is the recommended path.
func (f *Flags) applyDeprecated(d *db.DB, findings []deprecatedFinding, o DeprecatedOpts, withAI bool) error {
	byNumber := map[int]*deprecatedFinding{}
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
			return f.closeOneDeprecated(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "issues that lean on removed/deprecated things"
	p.GateLabel = "moot"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> issues as not planned in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close issues the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.deprecatedJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passDeprecated, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneDeprecated handles one candidate: card, the deprecated-close comment
// (citing the strongest match and its successor), and the close as not planned
// (or preview under dry-run, or the a/s ask when interactive).
func (f *Flags) closeOneDeprecated(d *db.DB, repo gh.Repo, fdg *deprecatedFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printDeprecatedCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := f.renderDeprecatedComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateDeprecatedClose, issue.StateNotPlanned)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as moot?", fdg.issue.Number), comment, fdg.issue.URL)
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
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reasonDeprecated)

	best := fdg.matches[0].removal
	what := best.Resource
	if best.Kind == db.RemovalKindProperty {
		what = best.Property + " on " + best.Resource
	}
	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reasonDeprecated,
		StateReason: issue.StateNotPlanned, Template: templateDeprecatedClose,
		Evidence:       map[string]string{"what": what, "action": best.Action, "source": best.Source, "successor": best.Successor},
		Source:         "deprecated",
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

// renderDeprecatedComment renders the close comment citing the strongest match.
func (f *Flags) renderDeprecatedComment(fdg *deprecatedFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateDeprecatedClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateDeprecatedClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateDeprecatedClose, err)
	}
	r := fdg.matches[0].removal
	data := struct {
		IsProperty   bool
		Name         string
		OnResource   string
		Removed      bool
		Major        int
		Successor    string
		SuccessorMD  string // successors backticked and or-joined for markdown
		SourceURL    string // where the removal/deprecation is documented
		SourceLabel  string // e.g. "v5.0 upgrade guide" | "v3.74.0 changelog"
		CurrentMajor int
	}{
		IsProperty: r.Kind == db.RemovalKindProperty, Name: r.Resource, OnResource: "",
		Removed: r.Action == db.RemovalRemoved, Major: r.Major, Successor: r.Successor,
		SourceURL: removalURL(r), CurrentMajor: f.CurrentMajor,
	}
	if v, ok := strings.CutPrefix(r.Source, "changelog "); ok {
		data.SourceLabel = v + " changelog"
	} else {
		data.SourceLabel = fmt.Sprintf("v%d.0 upgrade guide", r.Major)
	}
	if data.IsProperty {
		data.Name, data.OnResource = r.Property, r.Resource
	}
	// a removed resource is often superseded by several ("azurerm_linux_web_app
	// or azurerm_windows_web_app") — render them backticked and or-joined
	if r.Successor != "" {
		parts := strings.Split(r.Successor, ", ")
		for i := range parts {
			parts[i] = "`" + parts[i] + "`"
		}
		data.SuccessorMD = strings.Join(parts, " or ")
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateDeprecatedClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// deprecatedJudgeItems renders one judge block per finding: the issue's
// substance (body + comment digest) and every removed/deprecated thing it
// leans on, so the AI can tell a moot ask from an incidental mention.
func (f *Flags) deprecatedJudgeItems(d *db.DB, findings []deprecatedFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptDeprecated)
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
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "ISSUE COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		b.WriteString("REMOVED/DEPRECATED THINGS THE ISSUE REFERENCES:\n")
		for _, m := range fdg.matches {
			r := m.removal
			what := fmt.Sprintf("%s `%s`", strings.ReplaceAll(r.Kind, "-", " "), r.Resource)
			if r.Kind == db.RemovalKindProperty {
				what = fmt.Sprintf("property `%s` on `%s`", r.Property, r.Resource)
			}
			line := fmt.Sprintf("- %s: %s (%s)", what, r.Action, r.Source)
			if r.Successor != "" {
				line += ", use `" + r.Successor + "` instead"
			}
			fmt.Fprintf(&b, "%s\n", line)
			if r.Note != "" {
				fmt.Fprintf(&b, "  NOTE: %s\n", r.Note)
			}
			if m.quote != "" {
				fmt.Fprintf(&b, "  MATCHED ISSUE LINE: %s\n", m.quote)
			}
		}
		if len(fdg.alive) > 0 {
			fmt.Fprintf(&b, "RESOURCES THE ISSUE ALSO REFERENCES THAT ARE NOT REMOVED OR DEPRECATED: %s\n", strings.Join(fdg.alive, ", "))
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// removalURL deep-links a removal to where it is documented: the registry's
// rendered upgrade guide (anchored on the resource heading) for guide-sourced
// rows, the github release for changelog-sourced ones. Best effort — anchors
// follow the registry's heading-id scheme.
func removalURL(r db.Removal) string {
	if v, ok := strings.CutPrefix(r.Source, "changelog "); ok {
		return "https://github.com/hashicorp/terraform-provider-azurerm/releases/tag/" + v
	}
	return fmt.Sprintf("https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/guides/%d.0-upgrade-guide#%s", r.Major, r.Resource)
}

// matchLine returns the first line of t the matcher hits, for the card quote.
func matchLine(t string, re *regexp.Regexp) string {
	for line := range strings.SplitSeq(t, "\n") {
		if re.MatchString(line) {
			return text.TruncateRunes(text.OneLine(strings.TrimSpace(line)), 90)
		}
	}
	return ""
}

// printDeprecatedCard is one issue with every removed/deprecated thing it
// leans on, successors included, and the AI's moot score when judged.
func (f *Flags) printDeprecatedCard(fdg *deprecatedFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	shown := 0
	for _, m := range fdg.matches {
		if shown == 6 {
			cout.Printf("      <gray>… and %d more</>\n", len(fdg.matches)-shown)
			break
		}
		shown++
		r := m.removal
		actionTag := cli.TagYellow
		if r.Action == db.RemovalRemoved {
			actionTag = cli.TagRed
		}
		var b strings.Builder
		if r.Kind == db.RemovalKindProperty {
			fmt.Fprintf(&b, "<lightCyan>%s</> <gray>on</> %s", r.Property, r.Resource)
		} else {
			kind := strings.ReplaceAll(r.Kind, "-", " ")
			fmt.Fprintf(&b, "<lightCyan>%s</> <gray>(%s)</>", r.Resource, kind)
		}
		if ver, ok := strings.CutPrefix(r.Source, "changelog "); ok {
			fmt.Fprintf(&b, " <%s>%s</> <gray>in</> <lightMagenta>%s</>", actionTag, r.Action, ver)
		} else {
			fmt.Fprintf(&b, " <%s>%s</> <gray>in</> <lightMagenta>v%d.0</> <gray>(%s)</>", actionTag, r.Action, r.Major, r.Source)
		}
		if r.Successor != "" {
			fmt.Fprintf(&b, " <gray>· use</> <cyan>%s</>", r.Successor)
		}
		fmt.Fprintf(&b, " <darkGray>%s</>", removalURL(r))
		cout.Printf("      %s\n", b.String())
		if m.quote != "" {
			cout.Printf("        <gray>matched:</> %s\n", m.quote)
		}
	}
	if len(fdg.alive) > 0 {
		cout.Printf("      <gray>also references (not removed or deprecated):</> <green>%s</>\n",
			strings.Join(fdg.alive, "</> <gray>·</> <green>"))
	}
	cli.PrintVerdict(v)
}
