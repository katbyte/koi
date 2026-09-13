package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/katbyte/go-kt/clog"
)

// Signals is the per-issue output of the analyse pass: everything the rules
// engine and the review UI need to reason about an issue without re-parsing it.
type Signals struct {
	IssueNumber   int
	Kind          string // bug | enhancement | question | crash | ""
	VersionMajor  int    // 0 = undetermined
	VersionFull   string // e.g. "1.27.1" when known
	VersionSource string // label | template | body | comment
	VersionQuote  string // the text the version was parsed from

	Resources []string // azurerm_* names mentioned
	Service   string   // service/* label

	LastActivity        time.Time
	MaintainerCommented bool
	Participants        int

	// The newest provider major anyone mentions in a comment with azurerm context,
	// with the quote so a human can judge it. The checks' judges confirm semantics.
	NewestClaimMajor  int
	NewestClaimAt     time.Time
	NewestClaimQuote  string
	NewestClaimAuthor string

	OpenLinkedPRs      int
	MergedLinkedPRs    int
	MergedPRNumber     int
	MergedPRTitle      string
	MultiVersionLabels bool

	ComputedAt time.Time
}

// SaveSignals upserts the signals row for an issue.
func (d *DB) SaveSignals(s *Signals) error {
	resources, err := json.Marshal(s.Resources)
	if err != nil {
		return fmt.Errorf("marshalling resources for #%d: %w", s.IssueNumber, err)
	}

	_, err = d.Exec(`
		INSERT INTO signals (issue_number, kind, version_major, version_full, version_source, version_quote,
			resources, service, last_activity, maintainer_commented, participants,
			newest_claim_major, newest_claim_at, newest_claim_quote, newest_claim_author,
			open_linked_prs, merged_linked_prs, merged_pr_number, merged_pr_title,
			multi_version_labels, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(issue_number) DO UPDATE SET
			kind = excluded.kind, version_major = excluded.version_major,
			version_full = excluded.version_full, version_source = excluded.version_source,
			version_quote = excluded.version_quote, resources = excluded.resources,
			service = excluded.service, last_activity = excluded.last_activity,
			maintainer_commented = excluded.maintainer_commented, participants = excluded.participants,
			newest_claim_major = excluded.newest_claim_major, newest_claim_at = excluded.newest_claim_at,
			newest_claim_quote = excluded.newest_claim_quote, newest_claim_author = excluded.newest_claim_author,
			open_linked_prs = excluded.open_linked_prs, merged_linked_prs = excluded.merged_linked_prs,
			merged_pr_number = excluded.merged_pr_number, merged_pr_title = excluded.merged_pr_title,
			multi_version_labels = excluded.multi_version_labels, computed_at = excluded.computed_at`,
		s.IssueNumber, s.Kind, s.VersionMajor, s.VersionFull, s.VersionSource, s.VersionQuote,
		string(resources), s.Service, toDBTime(s.LastActivity), boolToInt(s.MaintainerCommented), s.Participants,
		s.NewestClaimMajor, toDBTime(s.NewestClaimAt), s.NewestClaimQuote, s.NewestClaimAuthor,
		s.OpenLinkedPRs, s.MergedLinkedPRs, s.MergedPRNumber, s.MergedPRTitle,
		boolToInt(s.MultiVersionLabels), toDBTime(s.ComputedAt))
	if err != nil {
		return fmt.Errorf("upserting signals for #%d: %w", s.IssueNumber, err)
	}
	return nil
}

// GetSignals returns the signals row for an issue, or nil when analyse hasn't run.
func (d *DB) GetSignals(number int) (*Signals, error) {
	row := d.QueryRow(`
		SELECT issue_number, kind, version_major, version_full, version_source, version_quote,
			resources, service, last_activity, maintainer_commented, participants,
			newest_claim_major, newest_claim_at, newest_claim_quote, newest_claim_author,
			open_linked_prs, merged_linked_prs, merged_pr_number, merged_pr_title,
			multi_version_labels, computed_at
		FROM signals WHERE issue_number = ?`, number)

	var s Signals
	var resources, lastActivity, claimAt, computedAt string
	var maintainer, multi int
	err := row.Scan(&s.IssueNumber, &s.Kind, &s.VersionMajor, &s.VersionFull, &s.VersionSource, &s.VersionQuote,
		&resources, &s.Service, &lastActivity, &maintainer, &s.Participants,
		&s.NewestClaimMajor, &claimAt, &s.NewestClaimQuote, &s.NewestClaimAuthor,
		&s.OpenLinkedPRs, &s.MergedLinkedPRs, &s.MergedPRNumber, &s.MergedPRTitle,
		&multi, &computedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading signals for #%d: %w", number, err)
	}

	s.MaintainerCommented, s.MultiVersionLabels = maintainer != 0, multi != 0
	s.LastActivity, s.NewestClaimAt, s.ComputedAt = fromDBTime(lastActivity), fromDBTime(claimAt), fromDBTime(computedAt)
	if err := json.Unmarshal([]byte(resources), &s.Resources); err != nil {
		clog.Log.Debugf("unparseable resources for #%d: %v", number, err)
	}
	return &s, nil
}

// Verdict is a cached AI judgement for one issue and pass.
type Verdict struct {
	IssueNumber int
	Pass        string // the check that judged it: fixed | resolved | legacy | ...
	PromptHash  string
	Model       string
	Verdict     string // raw JSON verdict
	Confidence  float64
	CreatedAt   time.Time
}

// SaveVerdict upserts the verdict for (issue, pass).
func (d *DB) SaveVerdict(v *Verdict) error {
	_, err := d.Exec(`
		INSERT INTO ai_verdicts (issue_number, pass, prompt_hash, model, verdict, confidence, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(issue_number, pass) DO UPDATE SET
			prompt_hash = excluded.prompt_hash, model = excluded.model, verdict = excluded.verdict,
			confidence = excluded.confidence, created_at = excluded.created_at`,
		v.IssueNumber, v.Pass, v.PromptHash, v.Model, v.Verdict, v.Confidence, toDBTime(v.CreatedAt))
	if err != nil {
		return fmt.Errorf("upserting %s verdict for #%d: %w", v.Pass, v.IssueNumber, err)
	}
	return nil
}

const verdictCols = "issue_number, pass, prompt_hash, model, verdict, confidence, created_at"

func scanVerdict(row interface{ Scan(...any) error }) (*Verdict, error) {
	var v Verdict
	var created string
	if err := row.Scan(&v.IssueNumber, &v.Pass, &v.PromptHash, &v.Model, &v.Verdict, &v.Confidence, &created); err != nil {
		return nil, err
	}
	v.CreatedAt = fromDBTime(created)
	return &v, nil
}

// GetVerdict returns the cached verdict for (issue, pass), or nil.
func (d *DB) GetVerdict(number int, pass string) (*Verdict, error) {
	row := d.QueryRow("SELECT "+verdictCols+" FROM ai_verdicts WHERE issue_number = ? AND pass = ?", number, pass)
	v, err := scanVerdict(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s verdict for #%d: %w", pass, number, err)
	}
	return v, nil
}

// Verdicts returns every cached verdict keyed by pass, then issue number — what
// the actions report needs to name the model behind each close.
func (d *DB) Verdicts() (map[string]map[int]*Verdict, error) {
	rows, err := d.Query("SELECT " + verdictCols + " FROM ai_verdicts")
	if err != nil {
		return nil, fmt.Errorf("querying verdicts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byPass := map[string]map[int]*Verdict{}
	for rows.Next() {
		v, err := scanVerdict(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning verdict: %w", err)
		}
		if byPass[v.Pass] == nil {
			byPass[v.Pass] = map[int]*Verdict{}
		}
		byPass[v.Pass][v.IssueNumber] = v
	}
	return byPass, rows.Err()
}

// Action statuses.
const (
	StatusProposed = "proposed"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusApplied  = "applied"
	StatusFailed   = "failed"
	StatusStale    = "stale"
	StatusSkipped  = "skipped"
	StatusReopened = "reopened"
)

// Action kinds.
const (
	ActionClose = "close"
	ActionKeep  = "keep"
	ActionHuman = "human"
)

// Action is one proposed (and later decided/applied) operation on an issue.
type Action struct {
	ID             int
	IssueNumber    int
	Action         string // close | keep | human
	Reason         string // reason code, e.g. legacy-bug
	StateReason    string // not_planned | completed (closes only)
	Template       string // comment template name (closes only)
	Evidence       map[string]string
	Confidence     float64
	Source         string // rules | ai | human
	Status         string
	ProposedAt     time.Time
	DecidedBy      string
	DecidedAt      time.Time
	IssueUpdatedAt time.Time // issue.updated_at snapshot for the staleness guard
	AppliedAt      time.Time
	Error          string
}

// ProposeAction inserts or refreshes the proposal for an issue. Rows a human has
// already decided (or that were applied) are never overwritten — re-running the
// pipeline must not clobber decisions.
func (d *DB) ProposeAction(a *Action) (bool, error) {
	var status string
	err := d.QueryRow("SELECT status FROM actions WHERE issue_number = ?", a.IssueNumber).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("checking action for #%d: %w", a.IssueNumber, err)
	}
	if err == nil && status != StatusProposed {
		clog.Log.Debugf("not overwriting %s action for #%d", status, a.IssueNumber)
		return false, nil
	}

	evidence, jerr := json.Marshal(a.Evidence)
	if jerr != nil {
		return false, fmt.Errorf("marshalling evidence for #%d: %w", a.IssueNumber, jerr)
	}

	_, err = d.Exec(`
		INSERT INTO actions (issue_number, action, reason, state_reason, template, evidence, confidence,
			source, status, proposed_at, issue_updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'proposed', ?, ?)
		ON CONFLICT(issue_number) DO UPDATE SET
			action = excluded.action, reason = excluded.reason, state_reason = excluded.state_reason,
			template = excluded.template, evidence = excluded.evidence, confidence = excluded.confidence,
			source = excluded.source, status = 'proposed', proposed_at = excluded.proposed_at,
			issue_updated_at = excluded.issue_updated_at, decided_by = '', decided_at = '', error = ''`,
		a.IssueNumber, a.Action, a.Reason, a.StateReason, a.Template, string(evidence), a.Confidence,
		a.Source, toDBTime(Now()), toDBTime(a.IssueUpdatedAt))
	if err != nil {
		return false, fmt.Errorf("upserting action for #%d: %w", a.IssueNumber, err)
	}
	return true, nil
}

const actionCols = `id, issue_number, action, reason, state_reason, template, evidence, confidence,
	source, status, proposed_at, decided_by, decided_at, issue_updated_at, applied_at, error`

func scanAction(row interface{ Scan(...any) error }) (*Action, error) {
	var a Action
	var evidence, proposed, decided, issueUpdated, applied string
	if err := row.Scan(&a.ID, &a.IssueNumber, &a.Action, &a.Reason, &a.StateReason, &a.Template, &evidence,
		&a.Confidence, &a.Source, &a.Status, &proposed, &a.DecidedBy, &decided, &issueUpdated, &applied, &a.Error); err != nil {
		return nil, err
	}
	a.ProposedAt, a.DecidedAt, a.IssueUpdatedAt, a.AppliedAt = fromDBTime(proposed), fromDBTime(decided), fromDBTime(issueUpdated), fromDBTime(applied)
	if err := json.Unmarshal([]byte(evidence), &a.Evidence); err != nil {
		clog.Log.Debugf("unparseable evidence for action %d: %v", a.ID, err)
	}
	return &a, nil
}

// GetAction returns the action row for an issue, or nil.
func (d *DB) GetAction(number int) (*Action, error) {
	row := d.QueryRow("SELECT "+actionCols+" FROM actions WHERE issue_number = ?", number)
	a, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading action for #%d: %w", number, err)
	}
	return a, nil
}

// ActionFilter narrows action queries; zero values mean "any".
type ActionFilter struct {
	Status        string
	Action        string
	Reason        string
	MinConfidence float64
	Limit         int
}

// Actions returns actions matching the filter, oldest issue first.
func (d *DB) Actions(f ActionFilter) ([]*Action, error) {
	q := "SELECT " + actionCols + " FROM actions WHERE 1=1"
	var args []any
	if f.Status != "" {
		q, args = q+" AND status = ?", append(args, f.Status)
	}
	if f.Action != "" {
		q, args = q+" AND action = ?", append(args, f.Action)
	}
	if f.Reason != "" {
		q, args = q+" AND reason = ?", append(args, f.Reason)
	}
	if f.MinConfidence > 0 {
		q, args = q+" AND confidence >= ?", append(args, f.MinConfidence)
	}
	q += " ORDER BY issue_number ASC"
	if f.Limit > 0 {
		q, args = q+" LIMIT ?", append(args, f.Limit)
	}

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying actions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var actions []*Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning action: %w", err)
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// DecideAction records a human decision on an action.
func (d *DB) DecideAction(id int, status, by string) error {
	if _, err := d.Exec("UPDATE actions SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?",
		status, by, toDBTime(Now()), id); err != nil {
		return fmt.Errorf("deciding action %d: %w", id, err)
	}
	return nil
}

// ReviseAction updates the proposal itself (reason edit during review).
func (d *DB) ReviseAction(id int, action, reason, stateReason, template string) error {
	if _, err := d.Exec("UPDATE actions SET action = ?, reason = ?, state_reason = ?, template = ?, source = 'human' WHERE id = ?",
		action, reason, stateReason, template, id); err != nil {
		return fmt.Errorf("revising action %d: %w", id, err)
	}
	return nil
}

// MarkApplied records the outcome of an apply attempt.
func (d *DB) MarkApplied(id int, status, errMsg string) error {
	if _, err := d.Exec("UPDATE actions SET status = ?, applied_at = ?, error = ? WHERE id = ?",
		status, toDBTime(Now()), errMsg, id); err != nil {
		return fmt.Errorf("marking action %d applied: %w", id, err)
	}
	return nil
}

// ChangelogEntry is one parsed changelog bullet.
type ChangelogEntry struct {
	Version  string
	Major    int
	Section  string // FEATURES | ENHANCEMENTS | BUG FIXES | OTHER
	Resource string
	Text     string
	PRNumber int
}

// ReplaceChangelog replaces the whole changelog table.
func (d *DB) ReplaceChangelog(entries []ChangelogEntry) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("beginning changelog tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM changelog"); err != nil {
		return fmt.Errorf("clearing changelog: %w", err)
	}
	for _, e := range entries {
		if _, err := tx.Exec("INSERT INTO changelog (version, major, section, resource, text, pr_number) VALUES (?, ?, ?, ?, ?, ?)",
			e.Version, e.Major, e.Section, e.Resource, e.Text, e.PRNumber); err != nil {
			return fmt.Errorf("inserting changelog entry: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing changelog tx: %w", err)
	}
	return nil
}

// CountChangelog returns the number of changelog entries stored.
func (d *DB) CountChangelog() (int, error) {
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM changelog").Scan(&n); err != nil {
		return 0, fmt.Errorf("counting changelog entries: %w", err)
	}
	return n, nil
}

// ChangelogVersionsForPR returns the distinct release versions whose changelog
// cites a PR number — proof a merged fix actually shipped, and in what release.
func (d *DB) ChangelogVersionsForPR(pr int) ([]string, error) {
	if pr == 0 {
		return nil, nil
	}
	rows, err := d.Query("SELECT DISTINCT version FROM changelog WHERE pr_number = ?", pr)
	if err != nil {
		return nil, fmt.Errorf("querying changelog for PR #%d: %w", pr, err)
	}
	defer func() { _ = rows.Close() }()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scanning changelog version: %w", err)
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// ChangelogTextFor returns the changelog bullet citing a PR (or issue) number
// in a given release, "" when there is none.
func (d *DB) ChangelogTextFor(version string, number int) (string, error) {
	var text string
	err := d.QueryRow("SELECT text FROM changelog WHERE version = ? AND pr_number = ? LIMIT 1", version, number).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading changelog text for %s #%d: %w", version, number, err)
	}
	return text, nil
}

// ChangelogFor returns changelog entries mentioning a resource.
func (d *DB) ChangelogFor(resource string) ([]ChangelogEntry, error) {
	rows, err := d.Query("SELECT version, major, section, resource, text, pr_number FROM changelog WHERE resource = ? ORDER BY major DESC, version DESC", resource)
	if err != nil {
		return nil, fmt.Errorf("querying changelog for %s: %w", resource, err)
	}
	defer func() { _ = rows.Close() }()

	var entries []ChangelogEntry
	for rows.Next() {
		var e ChangelogEntry
		if err := rows.Scan(&e.Version, &e.Major, &e.Section, &e.Resource, &e.Text, &e.PRNumber); err != nil {
			return nil, fmt.Errorf("scanning changelog entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// StatRow is a generic count grouped by up to two keys.
type StatRow struct {
	Key1  string
	Key2  string
	Count int
}

// ActionStats returns counts grouped by action/reason and status.
func (d *DB) ActionStats() ([]StatRow, error) {
	return d.statQuery("SELECT action || '/' || reason, status, COUNT(*) FROM actions GROUP BY 1, 2 ORDER BY 1, 2")
}

// SignalStats returns counts of open issues grouped by kind and version major.
func (d *DB) SignalStats() ([]StatRow, error) {
	return d.statQuery(`
		SELECT COALESCE(NULLIF(s.kind, ''), 'unknown'), 'v' || s.version_major, COUNT(*)
		FROM signals s JOIN issues i ON i.number = s.issue_number
		WHERE i.state = 'OPEN' GROUP BY 1, 2 ORDER BY 1, 2`)
}

func (d *DB) statQuery(q string) ([]StatRow, error) {
	rows, err := d.Query(q)
	if err != nil {
		return nil, fmt.Errorf("querying stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stats []StatRow
	for rows.Next() {
		var s StatRow
		if err := rows.Scan(&s.Key1, &s.Key2, &s.Count); err != nil {
			return nil, fmt.Errorf("scanning stat row: %w", err)
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}
