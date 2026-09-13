package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/katbyte/go-kt/clog"
)

// Issue states as GitHub's GraphQL API reports them.
const (
	IssueOpen   = "OPEN"
	IssueClosed = "CLOSED"
	PRMerged    = "MERGED"
)

type Issue struct {
	Number            int
	Title             string
	Body              string
	State             string // OPEN | CLOSED
	StateReason       string
	Author            string
	AuthorAssociation string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ClosedAt          time.Time
	Labels            []string
	CommentCount      int
	ThumbsUp          int
	ReactionsTotal    int
	URL               string
	FetchedAt         time.Time
}

// HasLabel reports whether the issue carries the exact label name.
func (i *Issue) HasLabel(name string) bool {
	return slices.Contains(i.Labels, name)
}

type Comment struct {
	ID                string
	IssueNumber       int
	Author            string
	AuthorAssociation string
	CreatedAt         time.Time
	Body              string
	URL               string
}

// IsMaintainer reports whether the comment author is a repo maintainer
// (member of the org or outside collaborator with push access).
func (c *Comment) IsMaintainer() bool {
	return c.AuthorAssociation == "MEMBER" || c.AuthorAssociation == "OWNER" || c.AuthorAssociation == "COLLABORATOR"
}

type Crossref struct {
	IssueNumber int
	RefRepo     string
	RefNumber   int
	IsPR        bool
	State       string // OPEN | CLOSED | MERGED
	Merged      bool
	MergedAt    time.Time
	Title       string
	WillClose   bool // the reference carries a closing keyword ("fixes #N")
}

// IssueBundle is everything fetched for one issue in one page.
type IssueBundle struct {
	Issue     Issue
	Comments  []Comment
	Crossrefs []Crossref
}

// SaveIssues writes a page of issues (with their comments and crossrefs) and the
// fetch cursor in a single transaction, so an interrupted fetch resumes cleanly.
func (d *DB) SaveIssues(bundles []IssueBundle, cursorKey, cursorVal string) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("beginning save tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := range bundles {
		if err := saveBundle(tx, &bundles[i]); err != nil {
			return err
		}
	}

	if cursorKey != "" {
		if _, err := tx.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", cursorKey, cursorVal); err != nil {
			return fmt.Errorf("saving cursor: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing save tx: %w", err)
	}
	return nil
}

func saveBundle(tx *sql.Tx, b *IssueBundle) error {
	i := &b.Issue
	labels, err := json.Marshal(i.Labels)
	if err != nil {
		return fmt.Errorf("marshalling labels for #%d: %w", i.Number, err)
	}

	_, err = tx.Exec(`
		INSERT INTO issues (number, title, body, state, state_reason, author, author_association,
			created_at, updated_at, closed_at, labels, comment_count, thumbs_up, reactions_total, url, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(number) DO UPDATE SET
			title = excluded.title, body = excluded.body, state = excluded.state,
			state_reason = excluded.state_reason, author = excluded.author,
			author_association = excluded.author_association, created_at = excluded.created_at,
			updated_at = excluded.updated_at, closed_at = excluded.closed_at,
			labels = excluded.labels, comment_count = excluded.comment_count,
			thumbs_up = excluded.thumbs_up, reactions_total = excluded.reactions_total,
			url = excluded.url, fetched_at = excluded.fetched_at`,
		i.Number, i.Title, i.Body, i.State, i.StateReason, i.Author, i.AuthorAssociation,
		toDBTime(i.CreatedAt), toDBTime(i.UpdatedAt), toDBTime(i.ClosedAt), string(labels),
		i.CommentCount, i.ThumbsUp, i.ReactionsTotal, i.URL, toDBTime(i.FetchedAt))
	if err != nil {
		return fmt.Errorf("upserting issue #%d: %w", i.Number, err)
	}

	// comments/crossrefs are replaced wholesale so deletions and edits upstream are reflected
	if _, err := tx.Exec("DELETE FROM comments WHERE issue_number = ?", i.Number); err != nil {
		return fmt.Errorf("clearing comments for #%d: %w", i.Number, err)
	}
	for _, c := range b.Comments {
		if _, err := tx.Exec(
			"INSERT OR REPLACE INTO comments (id, issue_number, author, author_association, created_at, body, url) VALUES (?, ?, ?, ?, ?, ?, ?)",
			c.ID, i.Number, c.Author, c.AuthorAssociation, toDBTime(c.CreatedAt), c.Body, c.URL); err != nil {
			return fmt.Errorf("inserting comment on #%d: %w", i.Number, err)
		}
	}

	if _, err := tx.Exec("DELETE FROM crossrefs WHERE issue_number = ?", i.Number); err != nil {
		return fmt.Errorf("clearing crossrefs for #%d: %w", i.Number, err)
	}
	for _, r := range b.Crossrefs {
		if _, err := tx.Exec(
			"INSERT OR REPLACE INTO crossrefs (issue_number, ref_repo, ref_number, is_pr, state, merged, merged_at, title, will_close) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			i.Number, r.RefRepo, r.RefNumber, boolToInt(r.IsPR), r.State, boolToInt(r.Merged), toDBTime(r.MergedAt), r.Title, boolToInt(r.WillClose)); err != nil {
			return fmt.Errorf("inserting crossref on #%d: %w", i.Number, err)
		}
	}

	return nil
}

// AppendComments adds extra comment pages for one issue (issues with >1 fetched page).
func (d *DB) AppendComments(comments []Comment) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("beginning comment tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range comments {
		if _, err := tx.Exec(
			"INSERT OR REPLACE INTO comments (id, issue_number, author, author_association, created_at, body, url) VALUES (?, ?, ?, ?, ?, ?, ?)",
			c.ID, c.IssueNumber, c.Author, c.AuthorAssociation, toDBTime(c.CreatedAt), c.Body, c.URL); err != nil {
			return fmt.Errorf("inserting comment on #%d: %w", c.IssueNumber, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing comment tx: %w", err)
	}
	return nil
}

const issueCols = `number, title, body, state, state_reason, author, author_association,
	created_at, updated_at, closed_at, labels, comment_count, thumbs_up, reactions_total, url, fetched_at`

func scanIssue(row interface{ Scan(...any) error }) (*Issue, error) {
	var i Issue
	var created, updated, closed, fetched, labels string
	if err := row.Scan(&i.Number, &i.Title, &i.Body, &i.State, &i.StateReason, &i.Author, &i.AuthorAssociation,
		&created, &updated, &closed, &labels, &i.CommentCount, &i.ThumbsUp, &i.ReactionsTotal, &i.URL, &fetched); err != nil {
		return nil, err
	}
	i.CreatedAt, i.UpdatedAt, i.ClosedAt, i.FetchedAt = fromDBTime(created), fromDBTime(updated), fromDBTime(closed), fromDBTime(fetched)
	if err := json.Unmarshal([]byte(labels), &i.Labels); err != nil {
		clog.Log.Debugf("unparseable labels for #%d: %v", i.Number, err)
	}
	return &i, nil
}

// GetIssue returns the issue or nil when it isn't in the db.
func (d *DB) GetIssue(number int) (*Issue, error) {
	row := d.QueryRow("SELECT "+issueCols+" FROM issues WHERE number = ?", number)
	i, err := scanIssue(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading issue #%d: %w", number, err)
	}
	return i, nil
}

// OpenIssues returns all open issues ordered oldest first.
func (d *DB) OpenIssues() ([]*Issue, error) {
	rows, err := d.Query("SELECT " + issueCols + " FROM issues WHERE state = 'OPEN' ORDER BY number ASC")
	if err != nil {
		return nil, fmt.Errorf("querying open issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var issues []*Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning issue: %w", err)
		}
		issues = append(issues, i)
	}
	return issues, rows.Err()
}

// ClosedIssues returns all closed issues in the db ordered oldest first —
// issues fetched while open that have since closed (the sync keeps them).
func (d *DB) ClosedIssues() ([]*Issue, error) {
	rows, err := d.Query("SELECT " + issueCols + " FROM issues WHERE state = 'CLOSED' ORDER BY number ASC")
	if err != nil {
		return nil, fmt.Errorf("querying closed issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var issues []*Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning issue: %w", err)
		}
		issues = append(issues, i)
	}
	return issues, rows.Err()
}

// CommentsFor returns an issue's comments ordered oldest first.
func (d *DB) CommentsFor(number int) ([]Comment, error) {
	rows, err := d.Query(
		"SELECT id, issue_number, author, author_association, created_at, body, url FROM comments WHERE issue_number = ? ORDER BY created_at ASC", number)
	if err != nil {
		return nil, fmt.Errorf("querying comments for #%d: %w", number, err)
	}
	defer func() { _ = rows.Close() }()

	var comments []Comment
	for rows.Next() {
		var c Comment
		var created string
		if err := rows.Scan(&c.ID, &c.IssueNumber, &c.Author, &c.AuthorAssociation, &created, &c.Body, &c.URL); err != nil {
			return nil, fmt.Errorf("scanning comment: %w", err)
		}
		c.CreatedAt = fromDBTime(created)
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// CrossrefsFor returns cross-referenced issues/PRs recorded for an issue.
func (d *DB) CrossrefsFor(number int) ([]Crossref, error) {
	rows, err := d.Query(
		"SELECT issue_number, ref_repo, ref_number, is_pr, state, merged, merged_at, title, will_close FROM crossrefs WHERE issue_number = ? ORDER BY ref_number ASC", number)
	if err != nil {
		return nil, fmt.Errorf("querying crossrefs for #%d: %w", number, err)
	}
	defer func() { _ = rows.Close() }()

	var refs []Crossref
	for rows.Next() {
		var r Crossref
		var isPR, merged, willClose int
		var mergedAt string
		if err := rows.Scan(&r.IssueNumber, &r.RefRepo, &r.RefNumber, &isPR, &r.State, &merged, &mergedAt, &r.Title, &willClose); err != nil {
			return nil, fmt.Errorf("scanning crossref: %w", err)
		}
		r.IsPR, r.Merged, r.MergedAt, r.WillClose = isPR != 0, merged != 0, fromDBTime(mergedAt), willClose != 0
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// IssueStates returns every known issue number and its state — the local half
// of the open-set reconcile against github.
func (d *DB) IssueStates() (map[int]string, error) {
	rows, err := d.Query("SELECT number, state FROM issues")
	if err != nil {
		return nil, fmt.Errorf("querying issue states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	states := map[int]string{}
	for rows.Next() {
		var number int
		var state string
		if err := rows.Scan(&number, &state); err != nil {
			return nil, fmt.Errorf("scanning issue state: %w", err)
		}
		states[number] = state
	}
	return states, rows.Err()
}

// IssueTitles returns every known issue number and its title — the light half
// of the issues table, for reports that only need to name an issue.
func (d *DB) IssueTitles() (map[int]string, error) {
	rows, err := d.Query("SELECT number, title FROM issues")
	if err != nil {
		return nil, fmt.Errorf("querying issue titles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	titles := map[int]string{}
	for rows.Next() {
		var number int
		var title string
		if err := rows.Scan(&number, &title); err != nil {
			return nil, fmt.Errorf("scanning issue title: %w", err)
		}
		titles[number] = title
	}
	return titles, rows.Err()
}

// CountIssues returns total and open issue counts.
func (d *DB) CountIssues() (total, open int, err error) {
	if err = d.QueryRow("SELECT COUNT(*), COALESCE(SUM(state = 'OPEN'), 0) FROM issues").Scan(&total, &open); err != nil {
		return 0, 0, fmt.Errorf("counting issues: %w", err)
	}
	return total, open, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
