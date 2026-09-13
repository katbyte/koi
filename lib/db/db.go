// Package db provides the SQLite store holding fetched issues, computed triage
// signals, AI verdicts, and the action queue. One file, inspectable with the
// sqlite3 CLI, safe to delete to start over.
package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/katbyte/go-kt/clog"
	_ "modernc.org/sqlite" // pure-go sqlite driver
)

type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the database at path and migrates the schema.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	// sqlite handles one writer at a time; a single connection avoids SQLITE_BUSY
	sdb.SetMaxOpenConns(1)

	d := &DB{sdb}
	if err := d.migrate(); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("migrating database %s: %w", path, err)
	}

	return d, nil
}

// migrations are applied in order; user_version records the last applied index+1.
var migrations = []string{`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE issues (
  number             INTEGER PRIMARY KEY,
  title              TEXT NOT NULL DEFAULT '',
  body               TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL DEFAULT '',
  state_reason       TEXT NOT NULL DEFAULT '',
  author             TEXT NOT NULL DEFAULT '',
  author_association TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL DEFAULT '',
  updated_at         TEXT NOT NULL DEFAULT '',
  closed_at          TEXT NOT NULL DEFAULT '',
  labels             TEXT NOT NULL DEFAULT '[]',
  comment_count      INTEGER NOT NULL DEFAULT 0,
  thumbs_up          INTEGER NOT NULL DEFAULT 0,
  reactions_total    INTEGER NOT NULL DEFAULT 0,
  url                TEXT NOT NULL DEFAULT '',
  fetched_at         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_issues_state ON issues(state);

CREATE TABLE comments (
  id                 TEXT PRIMARY KEY,
  issue_number       INTEGER NOT NULL,
  author             TEXT NOT NULL DEFAULT '',
  author_association TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL DEFAULT '',
  body               TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_comments_issue ON comments(issue_number, created_at);

CREATE TABLE crossrefs (
  issue_number INTEGER NOT NULL,
  ref_repo     TEXT NOT NULL,
  ref_number   INTEGER NOT NULL,
  is_pr        INTEGER NOT NULL DEFAULT 0,
  state        TEXT NOT NULL DEFAULT '',
  merged       INTEGER NOT NULL DEFAULT 0,
  merged_at    TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (issue_number, ref_repo, ref_number)
) WITHOUT ROWID;

CREATE TABLE changelog (
  version   TEXT NOT NULL,
  major     INTEGER NOT NULL,
  section   TEXT NOT NULL,
  resource  TEXT NOT NULL DEFAULT '',
  text      TEXT NOT NULL,
  pr_number INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_changelog_resource ON changelog(resource);

CREATE TABLE signals (
  issue_number         INTEGER PRIMARY KEY,
  kind                 TEXT NOT NULL DEFAULT '',
  version_major        INTEGER NOT NULL DEFAULT 0,
  version_full         TEXT NOT NULL DEFAULT '',
  version_source       TEXT NOT NULL DEFAULT '',
  version_quote        TEXT NOT NULL DEFAULT '',
  resources            TEXT NOT NULL DEFAULT '[]',
  service              TEXT NOT NULL DEFAULT '',
  last_activity        TEXT NOT NULL DEFAULT '',
  maintainer_commented INTEGER NOT NULL DEFAULT 0,
  participants         INTEGER NOT NULL DEFAULT 0,
  newest_claim_major   INTEGER NOT NULL DEFAULT 0,
  newest_claim_at      TEXT NOT NULL DEFAULT '',
  newest_claim_quote   TEXT NOT NULL DEFAULT '',
  newest_claim_author  TEXT NOT NULL DEFAULT '',
  open_linked_prs      INTEGER NOT NULL DEFAULT 0,
  merged_linked_prs    INTEGER NOT NULL DEFAULT 0,
  merged_pr_number     INTEGER NOT NULL DEFAULT 0,
  merged_pr_title      TEXT NOT NULL DEFAULT '',
  multi_version_labels INTEGER NOT NULL DEFAULT 0,
  computed_at          TEXT NOT NULL DEFAULT ''
);

CREATE TABLE ai_verdicts (
  issue_number INTEGER NOT NULL,
  pass         TEXT NOT NULL,
  prompt_hash  TEXT NOT NULL,
  model        TEXT NOT NULL DEFAULT '',
  verdict      TEXT NOT NULL DEFAULT '{}',
  confidence   REAL NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (issue_number, pass)
) WITHOUT ROWID;

CREATE TABLE actions (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_number     INTEGER NOT NULL UNIQUE,
  action           TEXT NOT NULL,
  reason           TEXT NOT NULL,
  state_reason     TEXT NOT NULL DEFAULT '',
  template         TEXT NOT NULL DEFAULT '',
  evidence         TEXT NOT NULL DEFAULT '{}',
  confidence       REAL NOT NULL DEFAULT 0,
  source           TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'proposed',
  proposed_at      TEXT NOT NULL DEFAULT '',
  decided_by       TEXT NOT NULL DEFAULT '',
  decided_at       TEXT NOT NULL DEFAULT '',
  issue_updated_at TEXT NOT NULL DEFAULT '',
  applied_at       TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_actions_status ON actions(status, reason);
`, `
CREATE TABLE milestones (
  number INTEGER PRIMARY KEY,
  title  TEXT NOT NULL,
  state  TEXT NOT NULL DEFAULT ''
);

-- the milestone audit scans ALL issues (open and closed) but only the light
-- fields it needs; it deliberately doesn't share tables with the triage flow
CREATE TABLE ms_issues (
  number       INTEGER PRIMARY KEY,
  title        TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT '',
  state_reason TEXT NOT NULL DEFAULT '',
  milestone    TEXT NOT NULL DEFAULT '',
  closed_at    TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL DEFAULT '',
  fetched_at   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE ms_fixes (
  issue_number INTEGER NOT NULL,
  pr_number    INTEGER NOT NULL,
  merged_at    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (issue_number, pr_number)
) WITHOUT ROWID;
`, `
-- comment web urls so evidence quotes can deep-link to the exact comment;
-- rows fetched before this column existed are backfilled by: koi fetch --full
ALTER TABLE comments ADD COLUMN url TEXT NOT NULL DEFAULT '';
`, `
-- how strongly a PR is tied to an issue: crossrefs learn whether the reference
-- carries a closing keyword (will_close), and milestone fixes get a link
-- strength: closed-by (the PR whose merge closed the issue) > linked
-- (closing-keyword reference) > mention (just referenced the issue number).
-- backfilled by: koi fetch --full and koi milestone --rescan
ALTER TABLE crossrefs ADD COLUMN will_close INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ms_fixes ADD COLUMN link TEXT NOT NULL DEFAULT 'mention';
`, `
-- light PR cache for the changelog check: every PR a changelog bullet cites,
-- with its current milestone. is_pr=0 marks bullet references that resolved to
-- issues, so they are fetched once and skipped forever after.
CREATE TABLE ms_prs (
  number     INTEGER PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT '',
  milestone  TEXT NOT NULL DEFAULT '',
  is_pr      INTEGER NOT NULL DEFAULT 1,
  fetched_at TEXT NOT NULL DEFAULT ''
);
`, `
-- full text for the AI match check: title + body of every candidate issue and
-- its evidence PRs, fetched on demand. issues and PRs share GitHub's number
-- space, so one table keyed by number covers both.
CREATE TABLE texts (
  number     INTEGER PRIMARY KEY,
  is_pr      INTEGER NOT NULL DEFAULT 0,
  state      TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  fetched_at TEXT NOT NULL DEFAULT ''
);
`, `
-- the last few comments of each fetched issue/PR, rendered as text: often the
-- closing comment explains WHY something was closed, which the AI judges need.
-- has_tail marks rows fetched since this column existed; older rows refetch.
ALTER TABLE texts ADD COLUMN tail TEXT NOT NULL DEFAULT '';
ALTER TABLE texts ADD COLUMN has_tail INTEGER NOT NULL DEFAULT 0;
`, `
-- tail lines gained full timestamps (same-day comments were landing on the
-- wrong side of the close split), a truncation note, and 15 comments over 8;
-- date-only tails refetch once to pick all of that up.
UPDATE texts SET has_tail = 0;
`, `
-- removed/deprecated resources, data sources, and properties, parsed from the
-- major-version upgrade guides and changelog DEPRECATIONS bullets. Rebuilt on
-- every fetch; koi deprecated scans open issues against it.
CREATE TABLE removals (
  kind      TEXT NOT NULL,
  resource  TEXT NOT NULL,
  property  TEXT NOT NULL DEFAULT '',
  action    TEXT NOT NULL,
  major     INTEGER NOT NULL DEFAULT 0,
  successor TEXT NOT NULL DEFAULT '',
  note      TEXT NOT NULL DEFAULT '',
  source    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_removals_resource ON removals(resource);
`, `
-- every resource and data source that exists in the provider RIGHT NOW, from
-- the website/docs listing. Rebuilt on every fetch; koi exists checks asked-for
-- things against it.
CREATE TABLE provider_docs (
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  PRIMARY KEY (kind, name)
) WITHOUT ROWID;
`, `
-- every argument/attribute each resource's documentation lists TODAY, parsed
-- from the doc pages (refetched only when the docs tree changes). koi exists
-- checks asked-for properties against it.
CREATE TABLE provider_doc_args (
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  arg  TEXT NOT NULL,
  PRIMARY KEY (kind, name, arg)
) WITHOUT ROWID;
`}

func (d *DB) migrate() error {
	var current int
	if err := d.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		clog.Log.Debugf("applying db migration %d", i+1)
		tx, err := d.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", i+1, err)
		}
		// PRAGMA cannot be parameterised; the value is a loop index, not user input
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("setting user_version %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", i+1, err)
		}
	}

	return nil
}

// GetMeta returns the value for key, or "" when unset.
func (d *DB) GetMeta(key string) (string, error) {
	var v string
	err := d.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading meta %s: %w", key, err)
	}
	return v, nil
}

// SetMeta upserts a meta key.
func (d *DB) SetMeta(key, value string) error {
	if _, err := d.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value); err != nil {
		return fmt.Errorf("writing meta %s: %w", key, err)
	}
	return nil
}

// Count returns the row count of one table. The name must come from a fixed
// in-code list, never user input.
func (d *DB) Count(table string) (int, error) {
	var n int
	if err := d.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", table, err)
	}
	return n, nil
}

// DeleteMeta removes a meta key.
func (d *DB) DeleteMeta(key string) error {
	if _, err := d.Exec("DELETE FROM meta WHERE key = ?", key); err != nil {
		return fmt.Errorf("deleting meta %s: %w", key, err)
	}
	return nil
}

// timestamps are stored as RFC3339 strings (UTC), "" for unset.

func toDBTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func fromDBTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		clog.Log.Debugf("unparseable timestamp %q in db", s)
		return time.Time{}
	}
	return t
}

// Now returns the current time in the storage format's precision.
func Now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}
