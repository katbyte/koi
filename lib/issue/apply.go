// The shared apply harness every close check rides: one banner, confirm,
// gate, close loop, and tally for resolved/legacy/fixed/duplicates/
// deprecated/comments/exists, so a change to the safety logic lands once
// instead of seven times.

package issue

import (
	"fmt"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/text"
)

// Apply-mode banner fragments, worded around the pass's verb ("close" for
// the checks, "label" for the labellers...).
func modePreviewEvery(one string) string { return fmt.Sprintf("<gray>previewing every %s</>", one) }

func modeEverything(verb string) string { return fmt.Sprintf("<gray>%s everything listed</>", verb) }
func modeConfirmEach(one string) string { return fmt.Sprintf("<gray>you confirm each %s</>", one) }

// DryRunTag marks a banner line when --dry-run is set.
func DryRunTag(dryRun bool) string {
	if dryRun {
		return " <yellow>(dry-run)</>"
	}
	return ""
}

// CloseFunc handles one candidate: card, comment, and the close itself (or a
// preview under dry-run, or the a/s ask when interactive), returning an
// Apply* outcome. v is nil under plain --apply.
type CloseFunc func(number int, v *Verdict, pos, total int, interactive bool) (int, error)

// JudgeFunc runs a pass's judge over its prepared blocks, wiring the
// harness's onReady/onBatch hooks into Judge.Blocks.
type JudgeFunc func(onReady func() (bool, error), onBatch func([]Judged) (bool, error)) error

// ApplyPass wires one check into the shared apply harness: everything that
// differs between the passes lives here, everything that must stay identical
// (the confidence gate, the counters, the confirms and the tally) lives in
// ApplyAll/ApplyAI.
type ApplyPass struct {
	Noun       string // what the banner says is being acted on ("duplicates", "legacy bugs", ...)
	GateLabel  string // what the gate tally calls its score ("match", "staleness", ...)
	ConfirmAll string // plain --apply's confirm question
	ConfirmAI  string // auto mode's confirm question
	AllMode    string // optional override for plain apply's mode fragment
	RepoTag    string

	// the pass's verb forms, defaulting to the checks' close/closing/closed —
	// the labellers set label/labelling/labelled.
	One  string // "close": what one action is called
	Verb string // "closing": the banner's present participle
	Done string // "closed": the tally's past participle

	DryRun    bool
	Yes       bool
	Auto      bool    // --apply-with-ai-auto
	Max       int     // stop after this many closes (0 = no cap)
	Threshold float64 // minimum confidence to act in auto/dry-run modes

	Title    func(number int) string
	URL      func(number int) string
	ScoreTag func(confidence float64) string
	Close    CloseFunc
}

// verbs fills the verb-form defaults in.
func (p *ApplyPass) verbs() (one, verb, done string) {
	one, verb, done = p.One, p.Verb, p.Done
	if one == "" {
		one = "close"
	}
	if verb == "" {
		verb = "closing"
	}
	if done == "" {
		done = "closed"
	}
	return one, verb, done
}

// ApplyAll is plain --apply: act on everything listed, no AI.
func (p *ApplyPass) ApplyAll(numbers []int) error {
	one, verb, _ := p.verbs()
	mode := modeEverything(verb)
	if p.AllMode != "" {
		mode = p.AllMode
	}
	if p.DryRun {
		mode = modePreviewEvery(one)
	}
	cout.Printf("%s <yellow>%d</> %s in %s <gray>·</> %s%s\n", verb, len(numbers), p.Noun, p.RepoTag, mode, DryRunTag(p.DryRun))

	if !p.DryRun && !p.Yes {
		ok, err := Confirm(p.ConfirmAll)
		if err != nil {
			return err
		}
		if !ok {
			cout.Printf("aborted\n")
			return nil
		}
	}

	closed, failed, previewed, skipped := 0, 0, 0, 0
	for n, number := range numbers {
		res, err := p.Close(number, nil, n+1, len(numbers), false)
		if err != nil {
			return err
		}
		switch res {
		case ApplySet:
			closed++
		case ApplyFailed:
			failed++
		case ApplyPreviewed:
			previewed++
		case ApplySkipped:
			skipped++
		}
		if !p.DryRun && p.Max > 0 && closed >= p.Max {
			cout.Printf("<gray>--max reached: %d %s, skipping the rest</>\n", p.Max, p.doneWord())
			break
		}
	}
	return p.summary(closed, skipped, 0, failed, previewed)
}

func (p *ApplyPass) doneWord() string { _, _, done := p.verbs(); return done }

// ApplyAI is --apply-with-ai[-auto], pipelined on the shared judge: batch N's
// candidates are reviewed and closed while batch N+1 is already off being
// scored, and auto mode's confirm comes right after batch 1 so answer time
// overlaps judging.
func (p *ApplyPass) ApplyAI(total int, judge JudgeFunc) error {
	interactive := !p.Auto && !p.DryRun
	one, verb, _ := p.verbs()

	mode := modeConfirmEach(one)
	switch {
	case p.DryRun:
		mode = fmt.Sprintf("<gray>previewing the ≥</> <green>%.2f</> <gray>gate</>", p.Threshold)
	case p.Auto:
		mode = fmt.Sprintf("<gray>auto-%s ≥</> <green>%.2f</>", verb, p.Threshold)
	}
	cout.Printf("%s up to <yellow>%d</> %s in %s <gray>·</> %s%s\n", verb, total, p.Noun, p.RepoTag, mode, DryRunTag(p.DryRun))

	pos, closed, failed, previewed, humanSkipped, skipped, below, unanswered := 0, 0, 0, 0, 0, 0, 0, 0
	process := func(ts []Judged) (bool, error) {
		for _, t := range ts {
			pos++
			v := t.Verdict
			switch {
			case v == nil:
				unanswered++
				cout.Printf("\n  <gray>%d/%d</> <gray>skip</> <cyan>#%d</> <yellow>no verdict</> %s\n",
					pos, total, t.Number, text.TruncateRunes(text.OneLine(p.Title(t.Number)), 70))
			case !interactive && v.Confidence < p.Threshold:
				below++
				cout.Printf("\n  <gray>%d/%d</> <gray>skip</> <cyan>#%d</> <%s>%.2f</> %s <darkGray>%s</>\n",
					pos, total, t.Number, p.ScoreTag(v.Confidence), v.Confidence,
					text.TruncateRunes(text.OneLine(p.Title(t.Number)), 80), p.URL(t.Number))
				cout.Printf("        <lightWhite>%s</>\n", text.OneLine(v.Reason))
			default:
				res, cerr := p.Close(t.Number, v, pos, total, interactive)
				if cerr != nil {
					return true, cerr
				}
				switch res {
				case ApplySet:
					closed++
				case ApplyFailed:
					failed++
				case ApplyPreviewed:
					previewed++
				case ApplySkipped:
					if interactive {
						humanSkipped++
					} else {
						skipped++
					}
				case ApplyQuit:
					cout.Printf("<gray>quitting — %d candidates left unreviewed</>\n", total-pos)
					return true, nil
				}
				if !p.DryRun && p.Max > 0 && closed >= p.Max {
					cout.Printf("<gray>--max reached: %d %s, skipping the rest</>\n", p.Max, p.doneWord())
					return true, nil
				}
			}
		}
		return false, nil
	}
	onReady := func() (bool, error) {
		if !p.Auto || p.DryRun || p.Yes {
			return true, nil
		}
		ok, err := Confirm(p.ConfirmAI)
		if err == nil && !ok {
			cout.Printf("aborted\n")
		}
		return ok, err
	}

	if err := judge(onReady, process); err != nil {
		return err
	}
	if below+unanswered > 0 {
		cout.Printf("\nAI %s gate: <fg=208>%d</> below %.2f · <yellow>%d</> unanswered\n", p.GateLabel, below, p.Threshold, unanswered)
	}
	return p.summary(closed, skipped, humanSkipped, failed, previewed)
}

// summary is the tally for both apply modes, worded around the pass's verb.
func (p *ApplyPass) summary(closed, skipped, humanSkipped, failed, previewed int) error {
	one, _, done := p.verbs()
	if p.DryRun {
		cout.Printf("\n<yellow>dry-run:</> %d %ss previewed, nothing changed\n", previewed, one)
		cout.Printf("<gray>drop</> <cyan>--dry-run</> <gray>to %s these, or switch to</> <cyan>--apply-with-ai</> <gray>to confirm each first</>\n", one)
		return nil
	}
	line := fmt.Sprintf("\n<green>%d %s</> · %d skipped", closed, done, skipped)
	if humanSkipped > 0 {
		line += fmt.Sprintf(" · %d skipped by you", humanSkipped)
	}
	cout.Printf("%s · %d failed\n", line, failed)
	if failed > 0 {
		return fmt.Errorf("%d %ss failed", failed, one)
	}
	return nil
}
