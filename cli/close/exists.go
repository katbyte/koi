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

const (
	passExists          = "exists"
	promptExists        = "enhancement-request-exists"
	templateExistsClose = "exists-close"
	reasonExists        = "request-exists"

	// classes by what the ask was, strongest first: the asked-for resource now
	// exists outright, or a property the ask names shipped after the ask.
	classExistsResource = "resource"
	classExistsProperty = "property"
)

// existsAsk is the title shape of a new-thing request — the resource class
// only fires on asks, not on bug reports that happen to name a resource.
var existsAsk = regexp.MustCompile(`(?i)\b(new (resource|data ?source)|support for|add(ing)? (support|resource|data ?source)|feature request|request.*resource|resource.*request)\b`)

// existsWord tokenises a title into lowercase words.
var existsWord = regexp.MustCompile(`[a-z][a-z0-9_]*`)

// existsAskFiller is what a title may trail after the thing it asks for
// without that thing ceasing to be the whole ask.
var existsAskFiller = map[string]bool{
	"resource": true, "resources": true, "data": true, "source": true, "sources": true,
	"datasource": true, "datasources": true, "support": true, "for": true, "to": true,
	"in": true, "the": true, "of": true, "a": true, "an": true, "azure": true,
	"azurerm": true, "terraform": true, "provider": true, "please": true, "new": true,
	"request": true, "feature": true,
}

// existsWholeAsk reports whether the title asks for name ITSELF — "New
// Resource: azurerm_x", "Support for azurerm_x" — with nothing substantive
// after it. A trailing qualifier ("Support for azurerm_monitor_action_group
// use AAD auth secure webhook") means the ask is that qualifier on a resource
// that already existed, and the resource being in the docs proves nothing.
func existsWholeAsk(title, name string) bool {
	lower := strings.ToLower(title)
	re := regexp.MustCompile("(?:new (?:resource|data ?source)s?:?|support (?:for|of)|add(?:ing)?(?: a| an| new)?(?: support for)?)\\s+(?:the )?`?" + regexp.QuoteMeta(name) + "`?\\b")
	m := re.FindStringIndex(lower)
	if m == nil {
		return false
	}
	for _, w := range existsWord.FindAllString(lower[m[1]:], -1) {
		if !existsAskFiller[w] {
			return false
		}
	}
	return true
}

// existsTitleWords is the title's vocabulary for matching documented
// arguments, singulars included and the azurerm_* tokens left out.
func existsTitleWords(title string) map[string]bool {
	words := map[string]bool{}
	for _, w := range existsWord.FindAllString(strings.ToLower(title), -1) {
		if strings.HasPrefix(w, "azurerm_") {
			continue
		}
		words[w] = true
		words[strings.TrimSuffix(w, "s")] = true
	}
	return words
}

// existsArgAsked reports whether a documented argument is what the title asks
// for — named outright, or spelled out in words ("use AAD auth secure webhook"
// asks for aad_auth). Single-word arguments are too loose to match on words.
func existsArgAsked(arg string, words map[string]bool) bool {
	parts := strings.Split(arg, "_")
	if len(parts) < 2 {
		// a one-word argument matches any title that happens to say the word —
		// "tags", "size", "delete" are in every other request and prove nothing
		return false
	}
	if words[arg] {
		return true
	}
	long := false
	for _, p := range parts {
		if !words[p] {
			return false
		}
		if len(p) >= 4 {
			long = true
		}
	}
	return long
}

// ExistsOpts configures the exists audit and its apply modes.
type ExistsOpts struct {
	Link                string // resource | property ("" = both classes)
	cli.FlagsApplyModes        // --apply / --apply-with-ai / --apply-with-ai-auto / --max
}

// existsEvidence is one shipped thing that appears to deliver the ask.
type existsEvidence struct {
	kind     string // resource | data-source | property
	name     string // azurerm_* or the property token
	resource string // owning resource for properties
	onKind   string // docs-page kind listing a property's owner ("" = resource)
	preAsk   bool   // arrived before the request was filed — already available
	version  string // release it arrived in
	pr       int    // changelog PR
	bullet   string // the changelog bullet text
	quote    string // the issue prose line the property matched ("" for resources)
}

// ownerKind is the docs-page kind that lists a property's owning resource — a
// data-source-only owner (azurerm_client_config) must not link /docs/resources/.
func (e *existsEvidence) ownerKind() string {
	if e.onKind != "" {
		return e.onKind
	}
	return db.DocKindResource
}

// existsFinding is one open enhancement whose ask appears to exist now.
//
// kindUnconfirmed marks the ones the rules could not identify as enhancements:
// the title reads as a request, so they are carried on that hypothesis and the
// AI is asked to confirm it before anything closes.
// evidence is sorted strongest first; the close comment cites the first.
type existsFinding struct {
	issue           *db.Issue
	kindUnconfirmed bool
	class           string
	evidence        []existsEvidence
}

// Exists finds OPEN enhancement requests whose ask already exists in the
// provider: the requested resource or data source is in the docs today and
// arrived after the ask, or a property the request names shipped in a later
// release. The AI judges whether what shipped actually delivers the specific
// request; the apply modes close as completed with the good news.
func (f *Flags) Exists(link string) error {
	o := ExistsOpts{Link: link, FlagsApplyModes: f.Modes}
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

	findings, counts, enh, err := f.collectExists(d, o.Link)
	if err != nil {
		return err
	}
	if enh == 0 {
		cout.Printf("nothing to check — run <cyan>koi fetch</> first\n")
		return nil
	}

	cout.Printf("\n<bold>%d of %d requests appear to already exist in the provider:</>\n", len(findings), enh)
	for _, c := range []struct{ class, tag string }{
		{classExistsResource, cli.TagGreen}, {classExistsProperty, cli.TagLightBlue},
	} {
		if n := counts[c.class]; n > 0 {
			cout.Printf("  <%s>%-10s</> <yellow>%d</>\n", c.tag, c.class, n)
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
		return f.applyExists(d, findings, o, true)
	case o.Apply:
		return f.applyExists(d, findings, o, false)
	}

	// report: score everything (pipelined, cached) and list surest first
	var verdicts map[int]*issue.Verdict
	if f.AI.Enabled {
		promptText, items, jerr := f.existsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		if verdicts, err = f.JudgeBlocks(d, passExists, promptText, items, nil, nil); err != nil {
			return err
		}
		slices.SortStableFunc(findings, func(a, b existsFinding) int {
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
		cout.Printf("<gray>--ai=false: listing without delivered scores</>\n")
	}

	for n := range findings {
		f.printExistsCard(&findings[n], n+1, len(findings), verdicts[findings[n].issue.Number])
	}
	cout.Printf("\nnext: <cyan>koi close exists --apply --dry-run</> to preview the closes, <cyan>--apply-with-ai</> to confirm each, <cyan>--apply-with-ai-auto</> to trust the scores\n")
	return nil
}

// collectExists builds the findings: open enhancements whose asked-for
// resource exists in the docs today and arrived after the ask, or whose prose
// names a property a later release shipped.
// The third return is how many open ENHANCEMENT requests were considered —
// the check only ever looks at those, so every open issue is the wrong
// denominator to report findings against.
func (f *Flags) collectExists(d *db.DB, link string) (findings []existsFinding, counts map[string]int, enh int, err error) {
	issues, err := d.OpenIssues()
	if err != nil {
		return nil, nil, 0, err
	}
	docs, err := d.ProviderDocs()
	if err != nil {
		return nil, nil, 0, err
	}
	docArgs, err := d.DocArgs()
	if err != nil {
		return nil, nil, 0, err
	}
	if len(docs) == 0 {
		cout.Printf("<yellow>no provider docs inventory — run koi fetch to list what exists</>\n")
		return nil, map[string]int{}, 0, nil
	}

	// when each resource/data source arrived, per the changelog's New
	// Resource/Data Source bullets — earliest PR wins
	type arrival struct {
		version string
		pr      int
		bullet  string
	}
	arrivals := map[string]arrival{}
	newThings, err := d.ChangelogLike("%New %ource%")
	if err != nil {
		return nil, nil, 0, err
	}
	// a real arrival bullet STARTS with "New (Beta) Resource/Data Source" and
	// its first backticked token is the arriving thing — prose like "creates a
	// new resource" and secondary mentions must not count, and v0.x bullets
	// cite foreign-repo PR numbers so they cannot date anything. Arrivals key
	// by kind|name: the resource existing forever while its DATA SOURCE
	// arrived recently must not read as the resource being new.
	reNewBullet := regexp.MustCompile("^\\**New (?:Beta )?(Resource|Data ?Source)s?[\\s:*]*`(?:data\\.)?(azurerm_[a-z0-9_]+)`")
	for _, e := range newThings {
		if e.Major == 0 {
			continue
		}
		m := reNewBullet.FindStringSubmatch(strings.TrimSpace(e.Text))
		if m == nil {
			continue
		}
		kind := db.DocKindResource
		if strings.HasPrefix(strings.ToLower(m[1]), "data") {
			kind = db.DocKindDataSource
		}
		key := kind + "|" + m[2]
		if a, ok := arrivals[key]; !ok || e.PRNumber < a.pr {
			arrivals[key] = arrival{version: e.Version, pr: e.PRNumber, bullet: text.TruncateRunes(text.OneLine(e.Text), 200)}
		}
	}

	// post-ask feature bullets grouped by resource, for the property class
	featureBullets := map[string][]db.ChangelogEntry{}
	all, err := d.ChangelogLike("%")
	if err != nil {
		return nil, nil, 0, err
	}
	for _, e := range all {
		if e.Resource == "" {
			continue
		}
		switch e.Section {
		case "ENHANCEMENTS", "FEATURES", "IMPROVEMENTS":
			featureBullets[e.Resource] = append(featureBullets[e.Resource], e)
		}
	}

	cout.Printf("scanning <yellow>%d</> open issues for enhancement requests against <yellow>%d</> existing resources/data sources...\n", len(issues), len(docs))
	reToken := regexp.MustCompile(`\b[a-z][a-z0-9_]*\b`)
	reBacktickTok := regexp.MustCompile("`([a-z0-9_.]+)`")

	// document frequency over enhancement prose, so generic property tokens
	// can't flood the property class (same cap as the deprecated check)
	type issueText struct {
		prose  string
		tokens map[string]bool
	}
	texts := map[int]issueText{}
	signals := map[int]*db.Signals{}
	var enhancements []*db.Issue
	unclassified, unconfirmed := 0, map[int]bool{}
	for _, i := range issues {
		s, serr := d.GetSignals(i.Number)
		if serr != nil {
			return nil, nil, 0, serr
		}
		if s == nil {
			continue
		}
		signals[i.Number] = s
		// the rules read kind from the labels and the issue template, and a
		// third of recent issues carry neither. An unlabelled issue whose
		// TITLE asks for something is carried as a probable request, with the
		// AI confirming that before anything closes.
		if s.Kind != "enhancement" {
			if s.Kind != "" || !existsAsk.MatchString(i.Title) {
				if s.Kind == "" {
					unclassified++
				}
				continue
			}
			unconfirmed[i.Number] = true
		}
		enhancements = append(enhancements, i)
		prose := i.Title + "\n" + issue.Prose(i.Body)
		set := map[string]bool{}
		for _, tok := range reToken.FindAllString(prose, -1) {
			set[tok] = true
		}
		texts[i.Number] = issueText{prose: prose, tokens: set}
	}
	cout.Printf("<yellow>%d</> requests to check <gray>(%d labelled enhancements, %d unlabelled but ask-shaped; %d bug/question/doc and %d unlabelled non-asks skipped)</>\n",
		len(enhancements), len(enhancements)-len(unconfirmed), len(unconfirmed),
		len(issues)-len(enhancements)-unclassified, unclassified)
	df := map[string]int{}
	for _, t := range texts {
		for tok := range t.tokens {
			df[tok]++
		}
	}
	tooGeneric := func(tok string) bool {
		return float64(df[tok]) > float64(len(enhancements))*deprecatedDFCap
	}

	counts = map[string]int{}
	for _, i := range enhancements {
		s := signals[i.Number]
		t := texts[i.Number]
		fdg := existsFinding{issue: i, kindUnconfirmed: unconfirmed[i.Number]}

		// resource class: an ask whose prose names a thing that exists in the
		// docs and whose arrival bullet postdates the ask. Both kinds are
		// checked and labelled honestly — the data source arriving while the
		// resource was asked for is weak evidence the AI weighs. Ubiquitous
		// names (everyone's config has a resource_group) are capped out.
		if existsAsk.MatchString(i.Title) {
			// data-source arrivals only count when the ask talks about a data
			// source — data sources for long-existing resources arrive all the
			// time and say nothing about resource-shaped asks
			lowerProse := strings.ToLower(t.prose)
			wantsDS := strings.Contains(lowerProse, "data source") || strings.Contains(lowerProse, "datasource")
			kinds := []string{db.DocKindResource}
			if wantsDS {
				kinds = append(kinds, db.DocKindDataSource)
			}
			for _, r := range s.Resources {
				// the resource must be the whole ask, not the thing a property
				// is being asked for ON — otherwise every "support for
				// azurerm_x <property>" reads as the resource itself shipping,
				// which it did long before the request was filed
				if tooGeneric(r) || !t.tokens[r] || !existsWholeAsk(i.Title, r) {
					continue
				}
				evidenced := false
				for _, kind := range kinds {
					key := kind + "|" + r
					a, arrived := arrivals[key]
					if !docs[key] || !arrived {
						continue
					}
					// an arrival predating the ask means the request was filed
					// for something already available — the citation is still
					// known, so keep it dated instead of "arrival not dated"
					fdg.evidence = append(fdg.evidence, existsEvidence{
						kind: kind, name: r, version: a.version, pr: a.pr, bullet: a.bullet,
						preAsk: a.pr <= i.Number,
					})
					fdg.class = classExistsResource
					evidenced = true
				}
				// docs-only: the docs listing is existence proof even without a
				// dateable arrival — including things that already existed when
				// the request was filed, which is still the ask being available.
				if !evidenced {
					for _, kind := range kinds {
						if docs[kind+"|"+r] {
							fdg.evidence = append(fdg.evidence, existsEvidence{
								kind: kind, name: r, bullet: "listed in the provider documentation (arrival not dated)",
							})
							fdg.class = classExistsResource
							break
						}
					}
				}
			}
		}

		// ownerDocKind is the docs page a property's owner actually lives on —
		// a data-source-only owner must not be linked as a resource
		ownerDocKind := func(r string) string {
			if !docs[db.DocKindResource+"|"+r] && docs[db.DocKindDataSource+"|"+r] {
				return db.DocKindDataSource
			}
			return db.DocKindResource
		}

		// property class: a post-ask feature bullet whose property token the
		// issue's prose asks about
		propSeen := map[string]bool{}
		for _, r := range s.Resources {
			for _, e := range featureBullets[r] {
				if e.PRNumber <= i.Number {
					continue
				}
				for _, m := range reBacktickTok.FindAllStringSubmatch(e.Text, -1) {
					tok := m[1]
					if !strings.Contains(tok, "_") || strings.HasPrefix(tok, "azurerm_") || tooGeneric(tok) || !t.tokens[tok] {
						continue
					}
					quote := matchLine(t.prose, regexp.MustCompile(`\b`+regexp.QuoteMeta(tok)+`\b`))
					propSeen[tok] = true
					fdg.evidence = append(fdg.evidence, existsEvidence{
						kind: db.RemovalKindProperty, name: tok, resource: r, onKind: ownerDocKind(r),
						version: e.Version, pr: e.PRNumber,
						bullet: text.TruncateRunes(text.OneLine(e.Text), 200), quote: quote,
					})
					if fdg.class == "" {
						fdg.class = classExistsProperty
					}
					break
				}
				if len(fdg.evidence) >= 6 {
					break
				}
			}
		}

		// property-in-docs: the property the TITLE asks for, as the resource's
		// documentation lists it today. Requests spell it out in prose ("use
		// AAD auth secure webhook") rather than naming the token, so match the
		// documented argument's own words — that is what makes the finding cite
		// aad_auth instead of the parent resource merely existing. Dated when a
		// post-ask feature bullet names it, cited as documented today otherwise.
		titleWords := existsTitleWords(i.Title)
		for _, r := range s.Resources {
			if !t.tokens[r] || tooGeneric(r) {
				continue
			}
			var matched []string
			argKind := map[string]string{}
			for _, kind := range []string{db.DocKindResource, db.DocKindDataSource} {
				for arg := range docArgs[kind+"|"+r] {
					if !propSeen[arg] && !tooGeneric(arg) && existsArgAsked(arg, titleWords) {
						propSeen[arg] = true
						matched = append(matched, arg)
						argKind[arg] = kind
					}
				}
			}
			slices.Sort(matched)
			for _, arg := range matched {
				if len(fdg.evidence) >= 6 {
					break
				}
				ev := existsEvidence{
					kind: db.RemovalKindProperty, name: arg, resource: r, onKind: argKind[arg],
					bullet: "listed in the documentation for " + r + " today (arrival not dated)",
					quote:  text.TruncateRunes(text.OneLine(i.Title), 120),
				}
				// earliest bullet naming it: the closest thing the changelog
				// has to when the argument arrived, rather than a later tweak
				// to it — an arrival predating the ask stays dated, flagged
				for _, e := range featureBullets[r] {
					if !strings.Contains(e.Text, "`"+arg+"`") {
						continue
					}
					if ev.pr == 0 || e.PRNumber < ev.pr {
						ev.version, ev.pr = e.Version, e.PRNumber
						ev.preAsk = e.PRNumber <= i.Number
						ev.bullet = text.TruncateRunes(text.OneLine(e.Text), 200)
					}
				}
				fdg.evidence = append(fdg.evidence, ev)
				if fdg.class == "" {
					fdg.class = classExistsProperty
				}
			}
		}

		if len(fdg.evidence) == 0 {
			continue
		}
		// resource evidence first — it is what the close comment cites
		slices.SortStableFunc(fdg.evidence, func(a, b existsEvidence) int {
			ar, br := 0, 0
			if a.kind != db.RemovalKindProperty {
				ar = 1
			}
			if b.kind != db.RemovalKindProperty {
				br = 1
			}
			return br - ar
		})
		if link != "" && fdg.class != link {
			continue
		}
		findings = append(findings, fdg)
		counts[fdg.class]++
	}

	rank := map[string]int{classExistsResource: 1, classExistsProperty: 0}
	slices.SortStableFunc(findings, func(a, b existsFinding) int {
		if d := rank[b.class] - rank[a.class]; d != 0 {
			return d
		}
		return a.issue.Number - b.issue.Number
	})
	return findings, counts, len(enhancements), nil
}

// registryDocURL links a resource/data source to its registry documentation.
func registryDocURL(kind, name string) string {
	section := "resources"
	if kind == db.DocKindDataSource {
		section = "data-sources"
	}
	return fmt.Sprintf("https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/%s/%s", section, strings.TrimPrefix(name, "azurerm_"))
}

// applyExists is both apply modes on the shared harness: plain --apply
// closes everything listed; --apply-with-ai[-auto] gates each close on the
// judge.
func (f *Flags) applyExists(d *db.DB, findings []existsFinding, o ExistsOpts, withAI bool) error {
	if !withAI {
		// nothing here confirms the hypothesis that an unlabelled issue is a
		// request, so those wait for an AI mode rather than closing on a guess
		held := 0
		kept := findings[:0]
		for _, fdg := range findings {
			if fdg.kindUnconfirmed {
				held++
				continue
			}
			kept = append(kept, fdg)
		}
		findings = kept
		if held > 0 {
			cout.Printf("<gray>holding back %d unlabelled issue(s): --apply-with-ai confirms what they are</>\n", held)
		}
	}

	byNumber := map[int]*existsFinding{}
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
			return f.closeOneExists(d, repo, byNumber[n], v, pos, total, throttle, interactive)
		})
	p.Noun = "delivered requests"
	p.GateLabel = "delivered"
	p.ConfirmAll = fmt.Sprintf("comment and close up to <yellow>%d</> requests as completed in %s?", len(findings), f.RepoTag())
	p.ConfirmAI = fmt.Sprintf("comment and close requests the AI scores ≥ <green>%.2f</> (up to <yellow>%d</> candidates) in %s?", p.Threshold, len(findings), f.RepoTag())

	if !withAI {
		return p.ApplyAll(numbers)
	}
	return p.ApplyAI(len(findings), func(onReady func() (bool, error), onBatch func([]issue.Judged) (bool, error)) error {
		promptText, items, jerr := f.existsJudgeItems(d, findings)
		if jerr != nil {
			return jerr
		}
		_, jerr = f.JudgeBlocks(d, passExists, promptText, items, onReady, onBatch)
		return jerr
	})
}

// closeOneExists handles one candidate: card, the good-news comment, and the
// close as completed (or preview under dry-run, or the a/s ask).
func (f *Flags) closeOneExists(d *db.DB, repo gh.Repo, fdg *existsFinding, v *issue.Verdict, pos, total int, throttle func(), ask bool) (int, error) {
	f.printExistsCard(fdg, pos, total, v)

	if rejected, err := cli.RejectedInReview(d, fdg.issue.Number); err != nil {
		return issue.ApplyFailed, err
	} else if rejected {
		cout.Printf("      <gray>a human rejected this close in review — skipped</>\n")
		return issue.ApplySkipped, nil
	}

	comment, err := f.renderExistsComment(fdg)
	if err != nil {
		return issue.ApplyFailed, err
	}
	if f.DryRun {
		cout.Printf("      <yellow>dry-run: would comment (%d chars, %s.md) then close as %s</>\n",
			len(comment), templateExistsClose, issue.StateCompleted)
		return issue.ApplyPreviewed, nil
	}

	if ask {
		res, perr := issue.AskClose(fmt.Sprintf("close <cyan>#%d</> as delivered?", fdg.issue.Number), comment, fdg.issue.URL)
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
	cout.QuietOnlyf("%d@closed@%s\n", fdg.issue.Number, reasonExists)

	best := fdg.evidence[0]
	what := best.name
	if best.kind == db.RemovalKindProperty {
		what = best.name + " on " + best.resource
	}
	a := &db.Action{
		IssueNumber: fdg.issue.Number, Action: db.ActionClose, Reason: reasonExists,
		StateReason: issue.StateCompleted, Template: templateExistsClose,
		Evidence:       map[string]string{"what": what, evidenceKeyVersion: best.version, "pr": fmt.Sprintf("#%d", best.pr)},
		Source:         passExists,
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

// renderExistsComment renders the good-news close citing the best evidence.
func (f *Flags) renderExistsComment(fdg *existsFinding) (string, error) {
	tt, err := assets.CommentTemplate(templateExistsClose)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(templateExistsClose).Parse(tt)
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templateExistsClose, err)
	}
	best := fdg.evidence[0]
	data := struct {
		IsProperty   bool
		Name         string
		OnResource   string
		Version      string
		PR           int
		DocsURL      string
		CurrentMajor int
	}{
		IsProperty: best.kind == db.RemovalKindProperty, Name: best.name, OnResource: best.resource,
		Version: best.version, PR: best.pr, CurrentMajor: f.CurrentMajor,
	}
	if data.IsProperty {
		data.DocsURL = registryDocURL(best.ownerKind(), best.resource)
	} else {
		data.DocsURL = registryDocURL(best.kind, best.name)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering template %s: %w", templateExistsClose, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// existsJudgeItems renders one judge block per finding: the request's
// substance and everything that shipped which appears to deliver it.
func (f *Flags) existsJudgeItems(d *db.DB, findings []existsFinding) (string, []issue.JudgeItem, error) {
	promptText, err := assets.Prompt(promptExists)
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
		if fdg.kindUnconfirmed {
			b.WriteString("KIND UNKNOWN: nothing labels this issue. Judge first whether it is a REQUEST for something at all — a bug report, a question or a discussion is not a request and scores 0.\n")
		}
		fmt.Fprintf(&b, "opened %s, last activity %s\n", fdg.issue.CreatedAt.Format("2006-01-02"), fdg.issue.UpdatedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "REQUEST BODY:\n%s\n", text.TruncateRunes(issue.CleanBody(fdg.issue.Body), cli.IssueBodyRunes))
		if picked := issue.DigestComments(comments, 8); len(picked) > 0 {
			fmt.Fprintf(&b, "REQUEST COMMENTS (%d of %d):\n", len(picked), len(comments))
			for _, c := range picked {
				fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", c.CreatedAt.Format("2006-01-02"), c.Author, c.AuthorAssociation,
					text.TruncateRunes(text.OneLine(issue.CleanBody(c.Body)), cli.CommentRunes))
			}
		}
		b.WriteString("WHAT SHIPPED THAT APPEARS TO DELIVER IT:\n")
		for _, e := range fdg.evidence {
			switch {
			case e.kind == db.RemovalKindProperty && e.version != "" && e.preAsk:
				fmt.Fprintf(&b, "- property `%s` on `%s` was ALREADY available when the request was filed (arrived in v%s, PR #%d): %s\n", e.name, e.resource, e.version, e.pr, e.bullet)
			case e.kind == db.RemovalKindProperty && e.version != "":
				fmt.Fprintf(&b, "- property `%s` on `%s`, shipped in v%s (PR #%d): %s\n", e.name, e.resource, e.version, e.pr, e.bullet)
			case e.kind == db.RemovalKindProperty:
				fmt.Fprintf(&b, "- property `%s` on `%s` is in the documentation TODAY (arrival not dated — it may have existed when the request was filed): %s\n", e.name, e.resource, registryDocURL(e.ownerKind(), e.resource))
			case e.version != "" && e.preAsk:
				fmt.Fprintf(&b, "- %s `%s` ALREADY existed when the request was filed (arrived in v%s, PR #%d): %s\n  DOCS: %s\n",
					strings.ReplaceAll(e.kind, "-", " "), e.name, e.version, e.pr, e.bullet, registryDocURL(e.kind, e.name))
			case e.version != "":
				fmt.Fprintf(&b, "- %s `%s` now EXISTS (arrived in v%s, PR #%d): %s\n  DOCS: %s\n",
					strings.ReplaceAll(e.kind, "-", " "), e.name, e.version, e.pr, e.bullet, registryDocURL(e.kind, e.name))
			default:
				fmt.Fprintf(&b, "- %s `%s` is in the provider documentation TODAY (arrival not dated — it may have existed when the request was filed)\n  DOCS: %s\n",
					strings.ReplaceAll(e.kind, "-", " "), e.name, registryDocURL(e.kind, e.name))
			}
			if e.quote != "" {
				fmt.Fprintf(&b, "  THE REQUEST'S PROSE MENTIONING IT: %s\n", e.quote)
			}
		}
		items = append(items, issue.JudgeItem{Number: fdg.issue.Number, Block: b.String()})
	}
	return promptText, items, nil
}

// printExistsCard is one candidate: the request and everything that shipped
// which appears to deliver it, with the AI's score when judged.
func (f *Flags) printExistsCard(fdg *existsFinding, pos, total int, v *issue.Verdict) {
	cout.Printf("\n  <gray>%d/%d</> <cyan>#%d</> %s <bold>%s</> <darkGray>%s</>\n",
		pos, total, fdg.issue.Number, text.StateTag(fdg.issue.State),
		text.TruncateRunes(text.OneLine(fdg.issue.Title), 90), f.IssueURL(fdg.issue.Number))
	if fdg.kindUnconfirmed {
		cout.Printf("      <yellow>unlabelled — carried as a probable request, the AI confirms it</>\n")
	}
	for n := range fdg.evidence {
		e := &fdg.evidence[n]
		var b strings.Builder
		switch {
		case e.kind == db.RemovalKindProperty && e.version != "":
			fmt.Fprintf(&b, "<lightBlue>%s</> <gray>on</> %s <green>shipped in</> <lightMagenta>v%s</> <gray>via</> PR <lightCyan>#%d</>", e.name, e.resource, e.version, e.pr)
		case e.kind == db.RemovalKindProperty:
			fmt.Fprintf(&b, "<lightBlue>%s</> <gray>on</> %s <green>in the docs today</> <darkGray>%s</>", e.name, e.resource, registryDocURL(e.ownerKind(), e.resource))
		case e.version != "":
			fmt.Fprintf(&b, "<green>%s</> <gray>(%s)</> <green>now exists</> <gray>— arrived in</> <lightMagenta>v%s</> <gray>via</> PR <lightCyan>#%d</> <darkGray>%s</>",
				e.name, strings.ReplaceAll(e.kind, "-", " "), e.version, e.pr, registryDocURL(e.kind, e.name))
		default:
			fmt.Fprintf(&b, "<green>%s</> <gray>(%s)</> <green>in the docs today</> <gray>— arrival not dated</> <darkGray>%s</>",
				e.name, strings.ReplaceAll(e.kind, "-", " "), registryDocURL(e.kind, e.name))
		}
		if e.preAsk {
			b.WriteString(" <gray>(predates the request)</>")
		}
		cout.Printf("      %s\n", b.String())
		cout.Printf("        <gray>changelog:</> %s\n", text.TruncateRunes(e.bullet, 110))
		if e.quote != "" {
			cout.Printf("        <gray>the ask:</> %s\n", e.quote)
		}
	}
	cli.PrintVerdict(v)
}
