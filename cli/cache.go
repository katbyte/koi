package cli

import (
	"fmt"
	"strings"

	"github.com/katbyte/go-kt/cout"
)

// cacheDomain is one clearable slice of the local db: fetched/derived data that
// koi can rebuild. Decisions (the actions table) are deliberately NOT a domain —
// they're human judgement, not cache.
type cacheDomain struct {
	name   string
	what   string   // one-line description for the listing
	tables []string // emptied by clear
	meta   []string // cursor/watermark meta keys reset by clear
}

var cacheDomains = []cacheDomain{
	{"ai", "AI verdicts from every check — cleared verdicts re-judge on the next run", []string{"ai_verdicts"}, nil},
	{"issues", "fetched open issues, comments, crossrefs, and derived signals", []string{"issues", "comments", "crossrefs", "signals"}, []string{"fetch_cursor", "last_sync"}},
	{"milestones", "the milestone scan: all-issue light rows, fix links, and the milestone list", []string{"ms_issues", "ms_fixes", "milestones"}, []string{"ms_scan_cursor", "ms_last_scan"}},
	{"prs", "changelog-cited PR details (changelog-check's cache)", []string{"ms_prs"}, nil},
	{"texts", "full issue/PR titles + bodies fetched for the AI match check", []string{"texts"}, nil},
	{"changelog", "parsed changelog entries for every release", []string{"changelog"}, nil},
}

// Cache lists the cache domains and their row counts, or clears one (or "all").
func (f *FlagData) Cache(domain string) error {
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	if domain == "" {
		cout.Printf("<bold>caches in %s:</>\n", f.DBPath)
		for _, dom := range cacheDomains {
			total := 0
			for _, table := range dom.tables {
				n, err := d.Count(table)
				if err != nil {
					return err
				}
				total += n
			}
			cout.Printf("  <cyan>%-10s</> <yellow>%7d</> rows <gray>· %s</>\n", dom.name, total, dom.what)
		}
		cout.Printf("clear one with <cyan>koi cache clear <domain></> (or <cyan>all</>)\n")
		return nil
	}

	var todo []cacheDomain
	for _, dom := range cacheDomains {
		if domain == "all" || domain == dom.name {
			todo = append(todo, dom)
		}
	}
	if len(todo) == 0 {
		names := make([]string, 0, len(cacheDomains))
		for _, dom := range cacheDomains {
			names = append(names, dom.name)
		}
		return fmt.Errorf("unknown cache %q (have: %s, all)", domain, strings.Join(names, ", "))
	}

	if f.DryRun {
		for _, dom := range todo {
			cout.Printf("<yellow>dry-run: would clear</> <cyan>%s</>\n", dom.name)
		}
		return nil
	}

	for _, dom := range todo {
		cleared := 0
		for _, table := range dom.tables {
			n, err := d.Count(table)
			if err != nil {
				return err
			}
			if _, err := d.Exec("DELETE FROM " + table); err != nil {
				return fmt.Errorf("clearing %s: %w", table, err)
			}
			cleared += n
		}
		for _, key := range dom.meta {
			if err := d.DeleteMeta(key); err != nil {
				return err
			}
		}
		cout.Printf("<fg=28>cleared</> <cyan>%s</> <gray>(%d rows)</>\n", dom.name, cleared)
	}
	return nil
}
