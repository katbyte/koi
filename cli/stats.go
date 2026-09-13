package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/text"
)

// Stats prints the triage funnel: what's fetched, what the signals say, and where
// every proposal stands.
func (f *FlagData) Stats() error {
	d, err := f.OpenDB()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	if err := f.EnsureAnalysed(d); err != nil {
		return err
	}

	total, open, err := d.CountIssues()
	if err != nil {
		return err
	}
	cout.Printf("<bold>issues:</> <yellow>%d</> fetched, <yellow>%d</> open\n", total, open)

	lastSync, err := d.GetMeta(MetaLastSync)
	if err != nil {
		return err
	}
	if lastSync != "" {
		cout.Printf("<gray>last sync: %s</>\n", lastSync)
	}

	signalStats, err := d.SignalStats()
	if err != nil {
		return err
	}
	if len(signalStats) > 0 {
		cout.Printf("\n<bold>open issues by kind × version major</> <gray>(v0 = undetermined)</>:\n")
		printStatTable(signalStats)
	} else {
		cout.Printf("\nno signals yet — run <cyan>koi fetch</>\n")
	}

	actionStats, err := d.ActionStats()
	if err != nil {
		return err
	}
	if len(actionStats) > 0 {
		cout.Printf("\n<bold>actions by proposal × status</>:\n")
		printStatTable(actionStats)
	} else {
		cout.Printf("\nno proposals yet — run <cyan>koi fetch</>\n")
	}

	return nil
}

// printStatTable pivots StatRows into key1 rows × key2 columns with totals.
func printStatTable(rows []db.StatRow) {
	cols := map[string]bool{}
	table := map[string]map[string]int{}
	rowTotals := map[string]int{}
	colTotals := map[string]int{}

	for _, r := range rows {
		cols[r.Key2] = true
		if table[r.Key1] == nil {
			table[r.Key1] = map[string]int{}
		}
		table[r.Key1][r.Key2] += r.Count
		rowTotals[r.Key1] += r.Count
		colTotals[r.Key2] += r.Count
	}

	colNames := text.SortedKeys(cols)
	rowNames := text.SortedKeys(table)

	// widths: first column sized to the longest row name
	rowW := len("total")
	for _, r := range rowNames {
		if len(r) > rowW {
			rowW = len(r)
		}
	}

	var header strings.Builder
	fmt.Fprintf(&header, "  %-*s", rowW, "")
	for _, c := range colNames {
		fmt.Fprintf(&header, "  %8s", c)
	}
	fmt.Fprintf(&header, "  %8s", "total")
	cout.Printf("<gray>%s</>\n", header.String())

	// rows sorted by total, biggest first
	sort.SliceStable(rowNames, func(a, b int) bool { return rowTotals[rowNames[a]] > rowTotals[rowNames[b]] })

	for _, r := range rowNames {
		var line strings.Builder
		fmt.Fprintf(&line, "  %-*s", rowW, r)
		for _, c := range colNames {
			if n := table[r][c]; n > 0 {
				fmt.Fprintf(&line, "  %8d", n)
			} else {
				fmt.Fprintf(&line, "  %8s", "·")
			}
		}
		fmt.Fprintf(&line, "  <yellow>%8d</>", rowTotals[r])
		cout.Printf("%s\n", line.String())
	}

	var footer strings.Builder
	fmt.Fprintf(&footer, "  %-*s", rowW, "total")
	grand := 0
	for _, c := range colNames {
		fmt.Fprintf(&footer, "  %8d", colTotals[c])
		grand += colTotals[c]
	}
	fmt.Fprintf(&footer, "  %8d", grand)
	cout.Printf("<gray>%s</>\n", footer.String())
}
