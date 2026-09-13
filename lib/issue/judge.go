package issue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/koi/lib/ai"
	"github.com/katbyte/koi/lib/db"
)

const (
	// judgeBatchSize is the default pairings per AI call — blocks carry full
	// issue and PR bodies, so batches stay small in favour of judgement
	// quality. Passes with even bigger blocks lower Judge.BatchSize further.
	judgeBatchSize = 10
	// judgePrefetch is how many batches are kept in flight ahead of the one
	// being reviewed. The AI CLI runs prefetched batches genuinely in
	// parallel, so this is the concurrency: a reviewer skimming cards faster
	// than one batch judges needs several in flight to stay off the AI's
	// critical path (kt hit this on the 377-candidate resolved wave — batch
	// boundaries stalled at depth 2). The cost is throwing away this much
	// judgement if the run is quit early — verdicts are cached, so even that
	// is only lost when the prompt or issues change before the next run.
	judgePrefetch = 4
	// maxConsecFails is how many batches may fail in a row before a run gives
	// up on the AI.
	maxConsecFails = 3
)

// Verdict is the AI's judgement of one issue↔evidence pairing: how likely the
// evidence actually resolves the issue, rjg-style.
type Verdict struct {
	Number     int     `json:"number"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// JudgeItem is one prepared block for the judge.
type JudgeItem struct {
	Number int
	Block  string
}

// Judged is one judged item handed to the caller's onBatch hook.
type Judged struct {
	Number  int
	Verdict *Verdict // nil when the batch failed or omitted it
}

// judgedBatch is one AI call's raw result, delivered over a channel by the
// background judge.
type judgedBatch struct {
	raw   string
	model string // the model that answered, when the CLI disclosed it
	err   error
}

// Judge scores prepared issue↔evidence blocks with the AI CLI and caches every
// verdict in ai_verdicts, model-aware: a verdict is a function of the model
// that produced it, so the cache must not serve one model's opinions to
// another.
type Judge struct {
	ai    ai.AI
	cmd   string
	model string // canonical, post-resolution
	ident string // how verdicts record who answered: cmd, or cmd/model
	db    *db.DB

	// BatchSize is pairings per AI call; set lower for passes whose blocks are
	// huge (docs ships page content). Does not affect the verdict cache — the
	// hash covers prompt and block, not batching.
	BatchSize int
}

// NewJudge resolves the canonical model first — an aliased model name (fable
// vs claude-fable-5) canonicalises here, before any cache comparison.
func NewJudge(d *db.DB, cmd, model string, timeout time.Duration) *Judge {
	a := ai.New(cmd, model, timeout)
	switch resolved, rerr := a.ResolveModel(); {
	case rerr != nil:
		cout.Errorf("<yellow>WARNING:</> resolving the AI model: %v — continuing as %q\n", rerr, aiIdent(cmd, model))
	case resolved != "":
		if model != "" && model != resolved {
			cout.Printf("<gray>model %s resolves to %s</>\n", model, resolved)
		}
		model = resolved
		a = ai.New(cmd, model, timeout)
	}
	return &Judge{ai: a, cmd: cmd, model: model, ident: aiIdent(cmd, model), db: d, BatchSize: judgeBatchSize}
}

// Model returns the canonical model the judge runs as.
func (j *Judge) Model() string { return j.model }

// Blocks scores prepared blocks, pipelined one batch ahead: the batch line
// prints before blocking on the result, and while a batch's verdicts are
// handled the next batch is already off being judged in the background.
//
// The hooks make it serve both reports and applies: onReady runs once after
// the first batch lands (the natural place for an auto-mode confirm, so answer
// time overlaps batch two; return false to abort), and onBatch receives each
// slice of judged items as it becomes ready — cached verdicts first, then
// batch by batch — for interleaved review/apply (return true to stop). Both
// may be nil.
func (j *Judge) Blocks(pass, promptText string, items []JudgeItem,
	onReady func() (bool, error), onBatch func([]Judged) (bool, error),
) (map[int]*Verdict, error) {
	verdicts := map[int]*Verdict{}
	type target struct {
		item    JudgeItem
		hash    string
		verdict *Verdict
	}
	var cached []Judged
	var uncached []*target
	for _, it := range items {
		hash := judgeHash(promptText, it.Block)
		if v, err := j.db.GetVerdict(it.Number, pass); err != nil {
			return nil, err
		} else if v != nil && v.PromptHash == hash && v.Model == j.ident {
			var mv Verdict
			if json.Unmarshal([]byte(v.Verdict), &mv) == nil {
				verdicts[it.Number] = &mv
				cached = append(cached, Judged{Number: it.Number, Verdict: &mv})
				continue
			}
		}
		uncached = append(uncached, &target{item: it, hash: hash})
	}

	cout.Printf("AI %s check: <yellow>%d</> pairings <gray>·</> <yellow>%d</> to evaluate via ai, <gray>%d already cached</> <gray>·</> <cyan>%s</> <gray>· model:</> <lightCyan>%s</>\n",
		pass, len(cached)+len(uncached), len(uncached), len(cached), j.cmd, j.model)

	buildPrompt := func(batch []*target) string {
		var prompt strings.Builder
		prompt.WriteString(promptText)
		for _, t := range batch {
			prompt.WriteString("\n")
			prompt.WriteString(t.item.Block)
		}
		return prompt.String()
	}
	launch := func(start int) <-chan judgedBatch {
		batch := uncached[start:min(start+j.BatchSize, len(uncached))]
		ch := make(chan judgedBatch, 1)
		go func() {
			raw, respModel, err := j.ai.PromptWithModel(buildPrompt(batch))
			ch <- judgedBatch{raw: raw, model: respModel, err: err}
		}()
		return ch
	}

	// save records one verdict under the identity that answered it.
	save := func(t *target, v *Verdict, ident string) error {
		raw, merr := json.Marshal(v)
		if merr != nil {
			return fmt.Errorf("marshalling verdict for #%d: %w", t.item.Number, merr)
		}
		if err := j.db.SaveVerdict(&db.Verdict{
			IssueNumber: t.item.Number, Pass: pass, PromptHash: t.hash,
			Model: ident, Verdict: string(raw), Confidence: v.Confidence, CreatedAt: db.Now(),
		}); err != nil {
			return err
		}
		t.verdict = v
		verdicts[t.item.Number] = v
		return nil
	}

	consecFails := 0
	harvest := func(start int, ch <-chan judgedBatch) error {
		end := min(start+j.BatchSize, len(uncached))
		cout.Printf("  batch <yellow>%d</>-<yellow>%d</> of <yellow>%d</>...", start+1, end, len(uncached))

		// say whether the pipeline actually paid off: a batch judged while the
		// last one was being reviewed costs nothing, one we sit and wait for is
		// the human going faster than the AI
		var res judgedBatch
		timing := "<gray>(judged while you reviewed)</>"
		select {
		case res = <-ch:
		default:
			// say so up front — a silent freeze here reads as the background
			// judging never having started, when it just isn't finished yet
			cout.Printf(" <gray>still judging, waiting...</>")
			started := time.Now()
			res = <-ch
			timing = fmt.Sprintf("<gray>(waited %s on the AI)</>", time.Since(started).Round(time.Second))
		}

		var batchVerdicts []Verdict
		err := res.err
		if err == nil {
			err = ai.ExtractJSON(res.raw, &batchVerdicts)
		}
		if err != nil {
			cout.Errorf(" <red>failed:</> %v\n", err)
			consecFails++
			if consecFails >= maxConsecFails {
				return fmt.Errorf("%d consecutive AI failures, aborting", consecFails)
			}
			return nil // targets stay verdict-less
		}
		consecFails = 0
		cout.Printf(" <green>ok</> %s\n", timing)
		// the model resolved up front; on mid-run drift (e.g. a CLI fallback)
		// the verdicts are another model's opinions — still used this run, but
		// cached under the answering model so they never come back as j.ident's
		ident := j.ident
		if res.model != "" && res.model != j.model {
			cout.Printf("  <yellow>note: this batch was answered by %s, not %s</>\n", res.model, j.model)
			ident = aiIdent(j.cmd, res.model)
		}

		byNumber := map[int]*Verdict{}
		for i := range batchVerdicts {
			byNumber[batchVerdicts[i].Number] = &batchVerdicts[i]
		}
		var missing []*target
		for _, t := range uncached[start:end] {
			v := byNumber[t.item.Number]
			if v == nil {
				missing = append(missing, t)
				continue
			}
			if err := save(t, v, ident); err != nil {
				return err
			}
		}
		if len(missing) == 0 {
			return nil
		}

		// a model occasionally drops an item from an otherwise-fine batch:
		// re-ask once with just the missing blocks before declaring them
		// unanswered (they stay uncached either way, so a rerun re-asks too)
		nums := make([]string, 0, len(missing))
		for _, t := range missing {
			nums = append(nums, fmt.Sprintf("#%d", t.item.Number))
		}
		cout.Printf("  <yellow>%s missing from the response — asking again for just %s</>\n",
			strings.Join(nums, ", "), map[bool]string{true: "it", false: "those"}[len(missing) == 1])
		raw, respModel, rerr := j.ai.PromptWithModel(buildPrompt(missing))
		var retryVerdicts []Verdict
		if rerr == nil {
			rerr = ai.ExtractJSON(raw, &retryVerdicts)
		}
		retryIdent := j.ident
		if respModel != "" && respModel != j.model {
			retryIdent = aiIdent(j.cmd, respModel)
		}
		byNumber = map[int]*Verdict{}
		for i := range retryVerdicts {
			byNumber[retryVerdicts[i].Number] = &retryVerdicts[i]
		}
		for _, t := range missing {
			v := byNumber[t.item.Number]
			switch {
			case rerr != nil || v == nil:
				cout.Errorf("  <yellow>#%d:</> no verdict in response\n", t.item.Number)
			default:
				if err := save(t, v, retryIdent); err != nil {
					return err
				}
			}
		}
		return nil
	}

	stopped := false
	emit := func(ts []Judged) error {
		if onBatch == nil || stopped || len(ts) == 0 {
			return nil
		}
		stop, err := onBatch(ts)
		stopped = stopped || stop
		return err
	}
	ready := func() (bool, error) {
		if onReady == nil {
			return true, nil
		}
		return onReady()
	}
	slice := func(start int) []Judged {
		end := min(start+j.BatchSize, len(uncached))
		out := make([]Judged, 0, end-start)
		for _, t := range uncached[start:end] {
			out = append(out, Judged{Number: t.item.Number, Verdict: t.verdict})
		}
		return out
	}

	if len(uncached) == 0 {
		if ok, err := ready(); err != nil || !ok {
			return verdicts, err
		}
		return verdicts, emit(cached)
	}

	// batch 1 in the foreground, then the confirm (auto mode's answer time
	// overlaps the prefetch), then cached + batch 1 + the pipeline
	type pending struct {
		start int
		ch    <-chan judgedBatch
	}
	var queue []pending
	launchAt := func(start int) {
		if start >= len(uncached) || stopped {
			return
		}
		queue = append(queue, pending{start: start, ch: launch(start)})
		if start > 0 {
			cout.Printf("<gray>starting batch</> <yellow>%d</>-<yellow>%d</> <gray>in the background...</>\n",
				start+1, min(start+j.BatchSize, len(uncached)))
		}
	}
	take := func() (pending, bool) {
		if len(queue) == 0 {
			return pending{}, false
		}
		p := queue[0]
		queue = queue[1:]
		return p, true
	}

	launchAt(0)
	first, _ := take()
	if err := harvest(first.start, first.ch); err != nil {
		return verdicts, err
	}
	// fill the pipeline: reviewing one batch should cover the next few being
	// judged, so a fast reviewer never waits on the AI
	for i := 1; i <= judgePrefetch; i++ {
		launchAt(i * j.BatchSize)
	}
	if ok, err := ready(); err != nil || !ok {
		return verdicts, err
	}
	if err := emit(cached); err != nil {
		return verdicts, err
	}
	if err := emit(slice(0)); err != nil {
		return verdicts, err
	}
	for start := j.BatchSize; start < len(uncached) && !stopped; start += j.BatchSize {
		next, ok := take()
		if !ok {
			launchAt(start)
			next, _ = take()
		}
		if err := harvest(next.start, next.ch); err != nil {
			return verdicts, err
		}
		// top the pipeline back up to depth
		launchAt(start + judgePrefetch*j.BatchSize)
		if err := emit(slice(start)); err != nil {
			return verdicts, err
		}
	}
	return verdicts, nil
}

func judgeHash(promptText, block string) string {
	h := sha256.Sum256([]byte(promptText + "|" + block))
	return hex.EncodeToString(h[:8])
}

// aiIdent is how a verdict records who answered: the CLI, and the model when
// one was set. A cached verdict only counts as a hit for the same identity.
func aiIdent(cmd, model string) string {
	if model == "" {
		return cmd
	}
	return cmd + "/" + model
}
