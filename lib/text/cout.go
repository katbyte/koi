package text

import (
	"fmt"
	"sort"
	"strings"

	"github.com/katbyte/go-kt/cout"
)

// StateTag colours a github issue/PR state for list lines: green when
// closed/merged, orange while open — padded so following text aligns.
func StateTag(state string) string {
	if state == "OPEN" {
		return "<fg=208>open</>  "
	}
	return "<green>" + fmt.Sprintf("%-6s", strings.ToLower(state)) + "</>"
}

// PrintCounts prints a sorted key → count tally, one indented line per key.
func PrintCounts(counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cout.Printf("  %-28s <yellow>%d</>\n", k, counts[k])
	}
}
