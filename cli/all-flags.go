// Package cli is koi's shared core: the flag data every command reads and the
// judge/apply/fetch plumbing the commands ride. The commands themselves live
// in the subpackages — cli/koi (root-level), cli/close (the checks),
// cli/milestone, cli/label — and main.go assembles the tree.
package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/koi/lib/ai"
	"github.com/katbyte/koi/lib/db"
	"github.com/katbyte/koi/lib/gh"
	"github.com/katbyte/koi/lib/issue"
)

// viper keys shared between flag registration, env binding, and the
// ValidateParams PreRunE lists in all-cmds.go.
const (
	ParamTokenGH = "token-gh"
	ParamRepo    = "repo"
)

// ValidateParams returns a PreRunE ensuring the named viper keys are non-empty.
func ValidateParams(params []string) func(cmd *cobra.Command, args []string) error {
	return func(_ *cobra.Command, _ []string) error {
		for _, p := range params {
			if viper.GetString(p) != "" {
				continue
			}
			return errors.New(p + " parameter can't be empty")
		}
		return nil
	}
}

type FlagData struct {
	GH FlagsGitHub `mapstructure:",squash"`
	AI FlagsAI     `mapstructure:",squash"`

	Cmd   FlagsCommands   `mapstructure:",squash"`
	Modes FlagsApplyModes `mapstructure:",squash"`

	DBPath        string `mapstructure:"db"`
	CurrentMajor  int    `mapstructure:"current-major"`
	KeepReactions int    `mapstructure:"keep-reactions"`
	As            string `mapstructure:"as"`
	DryRun        bool   `mapstructure:"dry-run"`
	Yes           bool   `mapstructure:"yes"`
	NoAutoFetch   bool   `mapstructure:"no-auto-fetch"`
	AutoReconcile bool   `mapstructure:"auto-reconcile"`

	// staticsTTL is set by AutoFetch for the duration of the call: fetch skips
	// the release-cadence artefacts refreshed more recently than this.
	staticsTTL time.Duration
}

type FlagsGitHub struct {
	Token string `mapstructure:"token-gh"`
	Repo  string `mapstructure:"repo"`
}

type FlagsAI struct {
	Enabled        bool   `mapstructure:"ai"`
	Cmd            string `mapstructure:"ai-cmd"`
	Model          string `mapstructure:"ai-model"`
	TimeoutMinutes int    `mapstructure:"ai-timeout"`
}

// FlagsCommands holds the flags that belong to a single command. They are
// registered on their command by the add*Flags helpers below (scoped help),
// and bindCommandFlags merges the executing command's flags into viper at run
// time, so the values arrive here through GetFlags like every other flag.
// Only the running command's fields carry values; the rest stay zero and
// unread. Flags that share a name across commands (--limit, --reason) share a
// viper key and fill every field carrying it — harmless for the same reason.
// Unlike the root flags none of these are bound to env vars.
type FlagsCommands struct {
	FetchFull     bool        `mapstructure:"full"`
	Review        FlagsReview `mapstructure:",squash"`
	Report        FlagsReport `mapstructure:",squash"`
	ApplyReason   string      `mapstructure:"reason"` // koi apply: only this reason code
	ReopenComment string      `mapstructure:"comment"`
	// koi close review: comment window (e.g. 10w, 2m), the manual-close sweep,
	// and whose comment marks a by-hand close
	ReviewLast       string         `mapstructure:"last"`
	ReviewExhaustive bool           `mapstructure:"exhaustive"`
	ReviewCloser     string         `mapstructure:"closer"`
	MS               FlagsMilestone `mapstructure:",squash"`
	LegacyMajors     []int          `mapstructure:"major"`
	Errors           FlagsErrors    `mapstructure:",squash"`
}

// FlagsErrors points the errors check at a provider checkout to search.
type FlagsErrors struct {
	ProviderSrc string `mapstructure:"provider-src"`
	ProviderRef string `mapstructure:"provider-ref"`
}

// FlagsReview filters which proposals koi review walks.
type FlagsReview struct {
	Reason        string  `mapstructure:"reason"`
	Action        string  `mapstructure:"action"` // close (default) | keep | human | "" for all
	MinConfidence float64 `mapstructure:"min-confidence"`
	Limit         int     `mapstructure:"limit"`
	ApproveAll    bool    `mapstructure:"approve-all"`
}

// FlagsReport configures koi close report and its subcommands.
type FlagsReport struct {
	Out    string `mapstructure:"out"`     // directory to write the report files into
	WithAI bool   `mapstructure:"with-ai"` // AI-score every candidate and sort surest first
	Limit  int    `mapstructure:"limit"`   // cap candidates per check, for cheap test runs (0 = all)
}

// FlagsMilestone configures the milestone audit's scan and output.
type FlagsMilestone struct {
	SkipScan bool   `mapstructure:"skip-scan"` // audit existing data without re-scanning
	Rescan   bool   `mapstructure:"rescan"`    // force a full re-walk
	CSV      string `mapstructure:"csv"`       // write the full audit to this csv ("" = don't)
	Bucket   string `mapstructure:"bucket"`    // list every finding in this bucket
}

// FlagsApplyModes is the tri-mode every check and the milestone audit share: act on
// the evidence alone, have the AI advise while a human confirms each one, or
// let the AI act unattended at or above a confidence. The flags are root
// persistent (registered in configureFlags) since nine commands share them;
// commands that mutate nothing simply never read them.
type FlagsApplyModes struct {
	Apply           bool    `mapstructure:"apply"`                    // act on the evidence, no AI
	ApplyWithAI     bool    `mapstructure:"apply-with-ai"`            // the AI scores, the human confirms each one
	ApplyWithAIAuto bool    `mapstructure:"apply-with-ai-auto-given"` // the AI scores, act at or above Threshold without asking
	Threshold       float64 `mapstructure:"apply-with-ai-auto"`       // auto-mode confidence floor
	Max             int     `mapstructure:"max"`                      // cap on mutations per run
}

func ConfigureFlags(root *cobra.Command) error {
	pflags := root.PersistentFlags()

	// GitHub Flags (FlagsGitHub)
	pflags.String(ParamTokenGH, "", "github token (consider exporting to GITHUB_TOKEN instead)")
	pflags.StringP(ParamRepo, "r", "hashicorp/terraform-provider-azurerm", "the owner/name of the repository to triage")

	// AI Flags (FlagsAI)
	pflags.Bool("ai", true, "use an AI CLI to judge the candidates each check finds")
	pflags.String("ai-cmd", "", "the AI CLI binary to invoke: claude, gemini, antigravity's agy, or IBM's bob (all run as <cmd> -p) — no default, set this or KOI_AI_CMD")
	pflags.String("ai-model", "", "the model to pass to the AI CLI via --model, e.g. fable, haiku, or a full model id — no default, set this or KOI_AI_MODEL")
	pflags.Int("ai-timeout", 10, "timeout, in minutes, for each AI CLI invocation")

	// General Flags (FlagData / Global)
	pflags.String("db", "issues.db", "path to the sqlite database")
	pflags.Int("current-major", 5, "the provider major version currently shipping")
	pflags.Int("keep-reactions", 20, "👍 count at or above which an issue is never auto-proposed for close")
	pflags.String("as", "", "who is making decisions (defaults to $USER); recorded on approvals")
	pflags.Bool("dry-run", false, "show what would happen without changing anything on GitHub")
	pflags.BoolP("yes", "y", false, "skip confirmation prompts")
	pflags.Bool("no-auto-fetch", false, "never touch the network for freshness — run against the local db as-is")
	pflags.Bool("auto-reconcile", false, "verify the local open set against github's real open issues during fetch — catches closes the search-index lag hides (report defaults this on; applies are guarded live instead)")

	// Apply-mode Flags (FlagsApplyModes) — shared by the checks, milestone, and apply
	pflags.Bool("apply", false, "act on the evidence with no AI: checks close everything they list, milestone sets every determined milestone")
	pflags.Bool("apply-with-ai", false, "the AI scores each candidate, you confirm each one interactively")
	pflags.Float64("apply-with-ai-auto", JudgeThreshold, fmt.Sprintf(
		"act on what the AI scores at or above this confidence (bare flag = %.2f, or --apply-with-ai-auto=0.85)", JudgeThreshold))
	pflags.Lookup("apply-with-ai-auto").NoOptDefVal = fmt.Sprintf("%g", JudgeThreshold)
	pflags.Int("max", 50, "maximum mutations (closes, milestone sets) to apply this run")

	// Output Flags
	pflags.Bool("quiet", false, "minimal machine-readable output")
	pflags.Bool("silent", false, "suppress all output")
	pflags.BoolP("verbose", "v", false, "show extra detail (full bodies, comments, reasoning)")

	// binding map for viper/pflag -> env vars (first entry wins when multiple are set)
	m := map[string][]string{
		ParamTokenGH:         {"GITHUB_TOKEN"},
		ParamRepo:            {"KOI_REPO"},
		"ai":                 {"KOI_AI"},
		"ai-cmd":             {"KOI_AI_CMD"},
		"ai-model":           {"KOI_AI_MODEL"},
		"ai-timeout":         {"KOI_AI_TIMEOUT"},
		"db":                 {"KOI_DB"},
		"current-major":      {"KOI_CURRENT_MAJOR"},
		"keep-reactions":     {"KOI_KEEP_REACTIONS"},
		"as":                 {"KOI_AS"},
		"dry-run":            {},
		"yes":                {},
		"apply":              {},
		"apply-with-ai":      {},
		"apply-with-ai-auto": {},
		"max":                {},
		"no-auto-fetch":      {"KOI_NO_AUTO_FETCH"},
		"auto-reconcile":     {"KOI_AUTO_RECONCILE"},
		"quiet":              {"KOI_OUTPUT_QUIET"},
		"silent":             {"KOI_OUTPUT_SILENT"},
		"verbose":            {},
	}

	for name, envs := range m {
		if err := viper.BindPFlag(name, pflags.Lookup(name)); err != nil {
			return fmt.Errorf("error binding '%s' flag: %w", name, err)
		}

		if len(envs) > 0 {
			if err := viper.BindEnv(append([]string{name}, envs...)...); err != nil {
				return fmt.Errorf("error binding '%s' to env '%v' : %w", name, envs, err)
			}
		}
	}

	viper.SetConfigName(".koi")
	viper.SetConfigType("env")
	if home, err := os.UserHomeDir(); err == nil {
		viper.AddConfigPath(home)
	}
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			clog.Log.Errorf("Error reading config file: %v", err)
		}
	}

	return nil
}

// BindCommandFlags merges the executing command's flags into viper so
// GetFlags sees them alongside the root flags configureFlags bound. Binding
// happens per run and only for the command actually executing, which is what
// lets same-named flags on different commands keep their own defaults.
func BindCommandFlags(cmd *cobra.Command) error {
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding %s flags: %w", cmd.Name(), err)
	}
	// --apply-with-ai-auto has NoOptDefVal: being given at all is the signal,
	// and viper only sees values — record presence under a synthetic key.
	if fl := cmd.Flags().Lookup("apply-with-ai-auto"); fl != nil {
		viper.Set("apply-with-ai-auto-given", fl.Changed)
	}
	return nil
}

// Per-command flag registration. These live here with their FlagsCommands
// structs so every flag koi accepts is declared in this file; cmds.go only
// builds the command tree.

// GetFlags returns the fully populated FlagData.
// We must unmarshal from Viper instead of using globally bound pflags variables
// because pflags only parses command-line arguments. Viper merges environment
// variables (and config files) on top of the CLI flags.
func GetFlags() *FlagData {
	var f FlagData
	if err := viper.Unmarshal(&f); err != nil {
		clog.Log.Fatalf("failed to unmarshal configuration: %v", err)
	}

	return &f
}

// RepoOwnerName splits the repo flag into owner and name.
func (f *FlagData) RepoOwnerName() (owner, name string, err error) {
	owner, name, ok := strings.Cut(f.GH.Repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("repo %q is not in owner/name form", f.GH.Repo)
	}
	return owner, name, nil
}

// OpenDB opens the configured sqlite database.
func (f *FlagData) OpenDB() (*db.DB, error) {
	return db.Open(f.DBPath)
}

// NewGraphQL returns the GraphQL client for bulk reads.
func (f *FlagData) NewGraphQL() *gh.Client {
	return gh.NewClient(f.GH.Token)
}

// NewRepo returns the REST client for mutations.
func (f *FlagData) NewRepo() (gh.Repo, error) {
	owner, name, err := f.RepoOwnerName()
	if err != nil {
		return gh.Repo{}, err
	}
	return gh.NewRepo(owner, name, f.GH.Token), nil
}

// RequireAI errors unless both the AI CLI and model are configured. There are
// deliberately NO defaults for either: which CLI and which model judge is
// always an explicit choice — verdicts are cached per model, and a silently
// assumed model would decide real closes.
func (f *FlagData) RequireAI() error {
	if f.AI.Cmd == "" {
		return errors.New("no AI CLI configured — set --ai-cmd or KOI_AI_CMD (claude, gemini, agy, or bob)")
	}
	if f.AI.Model == "" {
		return errors.New("no AI model configured — set --ai-model or KOI_AI_MODEL (e.g. fable, haiku, or a full model id)")
	}
	return nil
}

// RequireAIEarly is RequireAI hoisted ahead of the auto-fetch: every check
// calls it first so a missing CLI or model fails in milliseconds instead of
// after minutes of fetching. The run is going to judge unless the mode is
// plain --apply (evidence only) or --ai=false report mode (lists unscored);
// judging itself still calls RequireAI.
func (f *FlagData) RequireAIEarly() error {
	m := f.Modes
	if m.ApplyWithAI || m.ApplyWithAIAuto || (!m.Apply && f.AI.Enabled) {
		return f.RequireAI()
	}
	return nil
}

// NewAI returns the configured AI CLI wrapper.
func (f *FlagData) NewAI() ai.AI {
	return ai.New(f.AI.Cmd, f.AI.Model, time.Duration(f.AI.TimeoutMinutes)*time.Minute)
}

// RuleConfig returns the rules-engine configuration.
func (f *FlagData) RuleConfig() issue.RuleConfig {
	return issue.RuleConfig{CurrentMajor: f.CurrentMajor, KeepReactions: f.KeepReactions, Now: time.Now()}
}

// Decider returns who decisions are recorded as.
func (f *FlagData) Decider() string {
	if f.As != "" {
		return f.As
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// RepoTag is the triaged repo coloured for display: white owner / cyan name.
func (f *FlagData) RepoTag() string {
	if owner, name, err := f.RepoOwnerName(); err == nil {
		return fmt.Sprintf("<white>%s</>/<cyan>%s</>", owner, name)
	}
	return f.GH.Repo
}

// IssueURL builds the web url for an issue in the triaged repo.
func (f *FlagData) IssueURL(number int) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", f.GH.Repo, number)
}

// PRURL builds the web url for a PR in the triaged repo.
func (f *FlagData) PRURL(number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", f.GH.Repo, number)
}
