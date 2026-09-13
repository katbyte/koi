// Package ai shells out to an AI CLI for judgement calls, using the user's existing
// authentication rather than managing an API key. Supported CLI styles, detected from
// the binary name: claude (claude -p --output-format json, prompt on stdin, JSON result
// envelope), antigravity's agy (agy -p <prompt>, plain text output), IBM's bob
// (bob -p <prompt>, plain text output, no --model support), and google's gemini
// (gemini -p <prompt>, plain text output, -m model flag).
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katbyte/go-kt/clog"
)

type AI struct {
	Cmd     string        // the AI binary to invoke, e.g. "claude", "agy", or "bob"
	Model   string        // optional --model override (ignored by CLIs without model support)
	Timeout time.Duration // per-invocation timeout
}

func New(cmd, model string, timeout time.Duration) AI {
	return AI{Cmd: cmd, Model: model, Timeout: timeout}
}

// CLI invocation styles.
const (
	styleClaude = "claude" // claude -p --output-format json, prompt on stdin, JSON envelope
	styleAgy    = "agy"    // agy -p <prompt>, plain text output
	styleBob    = "bob"    // bob -p <prompt>, plain text output, no --model flag
	styleGemini = "gemini" // gemini -p <prompt>, plain text output, -m model flag
)

// style detects the CLI style from the binary name, defaulting to claude.
func (a AI) style() string {
	base := strings.ToLower(filepath.Base(a.Cmd))
	switch {
	case strings.Contains(base, styleAgy):
		return styleAgy
	case strings.Contains(base, styleBob):
		return styleBob
	case strings.Contains(base, styleGemini):
		return styleGemini
	default:
		return styleClaude
	}
}

// Prompt runs the AI CLI non-interactively with the prompt and returns the result text.
func (a AI) Prompt(prompt string) (string, error) {
	text, _, err := a.PromptWithModel(prompt)
	return text, err
}

// PromptWithModel is Prompt plus the model that actually answered, when the
// CLI's output discloses it: claude's JSON envelope names it (so a blank Model
// still learns what the CLI's default resolved to), the plain-text styles
// return "".
func (a AI) PromptWithModel(prompt string) (text, model string, err error) {
	switch a.style() {
	case styleAgy:
		return a.run(a.withModel([]string{"-p", prompt}), "", false)
	case styleBob:
		if a.Model != "" {
			clog.Log.Debugf("ignoring model %q: bob has no --model flag", a.Model)
		}
		return a.run([]string{"-p", prompt}, "", false)
	case styleGemini:
		args := []string{"-p", prompt}
		if a.Model != "" {
			args = append(args, "-m", a.Model)
		}
		return a.run(args, "", false)
	default:
		return a.run(a.withModel([]string{"-p", "--output-format", "json"}), prompt, true)
	}
}

// ResolveModel asks the CLI which model the configured (possibly blank or
// aliased) Model actually invokes, via a minimal prompt — claude's JSON
// envelope names the canonical model id. CLIs that don't disclose models
// return "".
func (a AI) ResolveModel() (string, error) {
	if a.style() != styleClaude {
		return "", nil
	}
	_, model, err := a.PromptWithModel("Reply with exactly: ok")
	return model, err
}

// withModel appends --model when one is set.
func (a AI) withModel(args []string) []string {
	if a.Model != "" {
		return append(args, "--model", a.Model)
	}
	return args
}

// run invokes the CLI with args, feeding stdin when non-empty. With envelope, claude's
// JSON envelope ({"result": "...", "is_error": bool, ...}) is unwrapped and the
// answering model reported from its modelUsage; otherwise the output is returned as
// trimmed plain text with no model.
func (a AI) run(args []string, stdin string, envelope bool) (text, model string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.Timeout)
	defer cancel()

	clog.Log.Debugf("running %s (%s style, %d args)", a.Cmd, a.style(), len(args))
	cmd := exec.CommandContext(ctx, a.Cmd, args...) //nolint:gosec // G204: the binary is user-configured on purpose (--ai-cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", "", fmt.Errorf("%s exited with %d: %s", a.Cmd, exitErr.ExitCode(), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("running %s: %w", a.Cmd, err)
	}

	if !envelope {
		return strings.TrimSpace(string(out)), "", nil
	}

	var env struct {
		Result     string `json:"result"`
		IsError    bool   `json:"is_error"`
		ModelUsage map[string]struct {
			OutputTokens int `json:"outputTokens"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return "", "", fmt.Errorf("parsing %s output envelope: %w", a.Cmd, err)
	}

	if env.IsError {
		return "", "", fmt.Errorf("%s returned an error: %s", a.Cmd, env.Result)
	}

	// the model that did the answering: most output tokens wins (ties broken
	// lexicographically so the pick is deterministic)
	model, best := "", -1
	for id, u := range env.ModelUsage {
		if u.OutputTokens > best || (u.OutputTokens == best && id < model) {
			model, best = id, u.OutputTokens
		}
	}
	return env.Result, model, nil
}

// ExtractJSON unmarshals a JSON object or array embedded in model output into v,
// tolerating markdown code fences and surrounding prose.
func ExtractJSON(s string, v any) error {
	s = strings.TrimSpace(s)

	// happy path: the whole response is valid JSON
	if json.Unmarshal([]byte(s), v) == nil {
		return nil
	}

	// otherwise pull out the first top-level array or object
	for _, pair := range []struct{ open, closing string }{{"[", "]"}, {"{", "}"}} {
		start := strings.Index(s, pair.open)
		end := strings.LastIndex(s, pair.closing)
		if start == -1 || end <= start {
			continue
		}
		if err := json.Unmarshal([]byte(s[start:end+1]), v); err == nil {
			return nil
		}
	}

	return fmt.Errorf("no valid JSON found in response: %.200s", s)
}
