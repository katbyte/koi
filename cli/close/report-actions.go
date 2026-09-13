package close

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/koi/cli"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/assets"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/issue"
	"github.com/katbyte/koi/lib/text"
)

// evidenceKeyAI is where every check parks the judge's reasoning on the action
// row it writes, so a close can always be explained after the fact.
const (
	evidenceKeyAI    = "ai"
	evidenceKeyClass = "class"
)

// actionsTakenStatuses are the statuses that mean koi acted on GitHub (or tried
// to). proposed/approved/rejected are decisions, not actions.
var actionsTakenStatuses = []string{db.StatusApplied, db.StatusFailed, db.StatusStale, db.StatusReopened}

// checkByReason names the check that owns each reason code. Most checks stamp
// their own name into action.source, but legacy closes are proposed by the
// rules engine and carry source "rules" — the reason is what identifies them,
// and the check name is also the ai_verdicts pass that judged them.
var checkByReason = map[string]string{
	issue.ReasonLegacyBug:     passLegacy,
	issue.ReasonFixedMergedPR: passFixed,
	reasonChangelogFixed:      passFixed,
	reasonComments:            passComments,
	reasonDeprecated:          passDeprecated,
	reasonExists:              passExists,
	reasonDuplicateOpen:       sourceDuplicates,
	reasonDuplicateResolved:   passResolved,
}

// actionEvidence is one key/value of an action's recorded evidence.
type actionEvidence struct {
	Key   string
	Value string
}

// actionItem is one issue koi acted on, with the AI decision behind it.
type actionItem struct {
	Number      int
	URL         string
	Title       string
	Status      string
	StatusKind  string
	StateReason string
	Reason      string
	Check       string
	Template    string
	Model       string
	AIScore     string // "" when the close was applied without the AI
	AIKind      string
	AIReason    string
	Evidence    []actionEvidence
	DecidedBy   string
	AppliedAt   string
	Error       string

	appliedAt time.Time
}

// actionSection groups the actions taken by reason code — one per way koi
// closes something.
type actionSection struct {
	Slug        string
	Check       string
	Reason      string
	StateReason string
	Verb        string
	Classes     []cli.ReportClass
	Items       []actionItem
}

type actionsData struct {
	Repo          string
	GeneratedAt   string
	Span          string
	Total         int
	Judged        int
	AvgConfidence string
	Models        []cli.ReportClass
	Sections      []actionSection
}

// ActionsTaken writes closed-<stamp>.html and .csv: every issue koi has
// closed (or tried to), the evidence that put it on the list, and the AI's
// score, reasoning, and model for each one. Pure db history — it never
// fetches. koi close report writes the same files on every run.
func (f *Flags) ActionsTaken() error {
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	htmlPath, csvPath, data, err := f.writeActionsTaken(d, f.Cmd.Report.Out, time.Now())
	if err != nil {
		return err
	}
	if htmlPath == "" {
		cout.Printf("no actions taken yet — nothing has been closed from this db\n")
		return nil
	}

	counts := map[string]int{}
	for _, s := range data.Sections {
		counts[s.Check] = len(s.Items)
	}
	cout.Printf("wrote <cyan>%s</> and <cyan>%s</> — <yellow>%d</> actions taken\n", htmlPath, csvPath, data.Total)
	text.PrintCounts(counts)
	if abs, aerr := filepath.Abs(htmlPath); aerr == nil {
		cout.Printf("<gray>open:</> <cyan>file://%s</>\n", abs)
	}
	return nil
}

// writeActionsTaken renders the closed ledger to closed-<stamp>.html/.csv in
// out, returning empty paths when nothing has been closed yet.
func (f *Flags) writeActionsTaken(d *db.DB, out string, now time.Time) (htmlPath, csvPath string, data *actionsData, err error) {
	actions, err := d.Actions(db.ActionFilter{})
	if err != nil {
		return "", "", nil, err
	}
	taken := make([]*db.Action, 0, len(actions))
	for _, a := range actions {
		if slices.Contains(actionsTakenStatuses, a.Status) {
			taken = append(taken, a)
		}
	}
	if len(taken) == 0 {
		return "", "", nil, nil
	}

	titles, err := d.IssueTitles()
	if err != nil {
		return "", "", nil, err
	}
	verdicts, err := d.Verdicts()
	if err != nil {
		return "", "", nil, err
	}

	items := make([]actionItem, 0, len(taken))
	for _, a := range taken {
		items = append(items, f.actionItem(a, titles, verdicts))
	}
	// newest first: the last wave is what a human wants to eyeball
	slices.SortStableFunc(items, func(a, b actionItem) int { return b.appliedAt.Compare(a.appliedAt) })

	d2 := f.actionsData(items, now)

	if err := os.MkdirAll(out, 0o750); err != nil {
		return "", "", nil, fmt.Errorf("creating %s: %w", out, err)
	}
	htmlPath = filepath.Join(out, cli.ReportFileName("closed", now))
	csvPath = strings.TrimSuffix(htmlPath, ".html") + ".csv"
	if err := writeActionsHTML(htmlPath, &d2); err != nil {
		return "", "", nil, err
	}
	if err := writeActionsCSV(csvPath, items); err != nil {
		return "", "", nil, err
	}
	return htmlPath, csvPath, &d2, nil
}

// actionItem renders one action row: the issue it touched, the evidence that
// listed it, and the AI verdict behind the decision.
func (f *Flags) actionItem(a *db.Action, titles map[int]string, verdicts map[string]map[int]*db.Verdict) actionItem {
	check := checkForAction(a)
	item := actionItem{
		Number: a.IssueNumber, URL: f.IssueURL(a.IssueNumber),
		Title:       text.OneLine(titles[a.IssueNumber]),
		Status:      a.Status,
		StatusKind:  actionStatusKind(a.Status),
		StateReason: cli.OrDash(a.StateReason),
		Reason:      a.Reason,
		Check:       check,
		Template:    cli.OrDash(a.Template),
		DecidedBy:   a.DecidedBy,
		Error:       a.Error,
		appliedAt:   a.AppliedAt,
	}
	if !a.AppliedAt.IsZero() {
		item.AppliedAt = a.AppliedAt.Format("2006-01-02 15:04")
	}

	v := verdicts[check][a.IssueNumber]
	if v != nil {
		item.Model = v.Model
	}
	if a.Confidence > 0 {
		item.AIScore = fmt.Sprintf("%.2f", a.Confidence)
		item.AIKind = cli.ReportAIKind(a.Confidence)
	}
	item.AIReason = text.OneLine(a.Evidence[evidenceKeyAI])
	if item.AIReason == "" && v != nil {
		// no reasoning on the row (an older close): fall back to the cached verdict
		var mv issue.Verdict
		if err := json.Unmarshal([]byte(v.Verdict), &mv); err == nil {
			item.AIReason = text.OneLine(mv.Reason)
		}
	}

	for _, k := range text.SortedKeys(a.Evidence) {
		if k == evidenceKeyAI || a.Evidence[k] == "" {
			continue
		}
		item.Evidence = append(item.Evidence, actionEvidence{Key: k, Value: a.Evidence[k]})
	}
	return item
}

// actionsData tallies the items and groups them into per-reason sections,
// biggest first.
func (f *Flags) actionsData(items []actionItem, now time.Time) actionsData {
	data := actionsData{
		Repo: f.GH.Repo, GeneratedAt: now.Format("2006-01-02 15:04"), Total: len(items),
	}

	var sum float64
	models, states := map[string]int{}, map[string]map[string]int{}
	bySlug := map[string]*actionSection{}
	for _, item := range items {
		if item.AIScore != "" {
			score, _ := strconv.ParseFloat(item.AIScore, 64)
			sum += score
			data.Judged++
		}
		if item.Model != "" {
			models[item.Model]++
		}

		s := bySlug[item.Reason]
		if s == nil {
			s = &actionSection{Slug: item.Reason, Check: item.Check, Reason: item.Reason, Verb: "closed"}
			bySlug[item.Reason] = s
			states[item.Reason] = map[string]int{}
		}
		s.Items = append(s.Items, item)
		states[item.Reason][item.StateReason]++
		s.Classes = bumpClass(s.Classes, item.Status, actionStatusKind(item.Status))
	}
	if data.Judged > 0 {
		data.AvgConfidence = fmt.Sprintf("%.2f", sum/float64(data.Judged))
	}
	for _, name := range text.SortedKeys(models) {
		data.Models = append(data.Models, cli.ReportClass{Name: name, Count: models[name], Kind: cli.KindDim})
	}

	for _, s := range bySlug {
		s.StateReason = strings.Join(text.SortedKeys(states[s.Slug]), " / ")
		data.Sections = append(data.Sections, *s)
	}
	sort.Slice(data.Sections, func(i, j int) bool {
		if len(data.Sections[i].Items) != len(data.Sections[j].Items) {
			return len(data.Sections[i].Items) > len(data.Sections[j].Items)
		}
		return data.Sections[i].Slug < data.Sections[j].Slug
	})

	// the reporting window, oldest to newest, from the already-sorted items
	if first, last := items[len(items)-1].AppliedAt, items[0].AppliedAt; first != "" && last != "" {
		data.Span = first + " and " + last
		if first == last {
			data.Span = first
		}
	}
	return data
}

// bumpClass increments the named tally, appending it the first time it is seen
// so the pills keep the order they were encountered in.
func bumpClass(classes []cli.ReportClass, name, kind string) []cli.ReportClass {
	for i := range classes {
		if classes[i].Name == name {
			classes[i].Count++
			return classes
		}
	}
	return append(classes, cli.ReportClass{Name: name, Count: 1, Kind: kind})
}

// checkForAction names the check behind an action: the checks stamp themselves
// into source, the rules engine stamps "rules", so the reason code decides.
func checkForAction(a *db.Action) string {
	if check, ok := checkByReason[a.Reason]; ok {
		return check
	}
	return a.Source
}

func actionStatusKind(status string) string {
	switch status {
	case db.StatusApplied:
		return cli.KindOK
	case db.StatusFailed:
		return cli.KindBad
	case db.StatusReopened:
		return cli.KindWarn
	default:
		return cli.KindMid
	}
}

func writeActionsHTML(path string, data *actionsData) error {
	tmpl, err := template.New("actions").Parse(assets.Styles() + assets.ActionsHTML())
	if err != nil {
		return fmt.Errorf("parsing actions template: %w", err)
	}

	out, err := os.Create(path) //nolint:gosec // G304: user-chosen output path is the point
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = out.Close() }()

	if err := tmpl.Execute(out, data); err != nil {
		return fmt.Errorf("rendering actions report: %w", err)
	}
	return nil
}

func writeActionsCSV(path string, items []actionItem) error {
	out, err := os.Create(path) //nolint:gosec // G304: user-chosen output path is the point
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = out.Close() }()

	w := csv.NewWriter(out)
	if err := w.Write([]string{
		cli.CSVColNumber, "status", "check", "reason", "state_reason", "template", "confidence",
		"ai_model", "ai_reason", "evidence", "decided_by", "applied_at", "error", cli.CSVColTitle, cli.CSVColURL,
	}); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	for _, item := range items {
		evidence := make([]string, 0, len(item.Evidence))
		for _, e := range item.Evidence {
			evidence = append(evidence, e.Key+"="+e.Value)
		}
		row := []string{
			strconv.Itoa(item.Number), item.Status, item.Check, item.Reason, item.StateReason, item.Template,
			item.AIScore, item.Model, item.AIReason, strings.Join(evidence, "; "),
			item.DecidedBy, item.AppliedAt, item.Error, item.Title, item.URL,
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	w.Flush()
	return w.Error()
}
