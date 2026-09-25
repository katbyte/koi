package cout

// Flags is the set of verbosity flags katbyte tools expose. Embed it in a
// tool's flag struct with `mapstructure:",squash"` and viper fills it from the
// "silent", "json", "quiet" and "verbose" keys; a tool registers only the flags
// it offers and keeps ownership of their names, shorthands and help text.
type Flags struct {
	Silent  bool `mapstructure:"silent"`
	JSON    bool `mapstructure:"json"`
	Quiet   bool `mapstructure:"quiet"`
	Verbose bool `mapstructure:"verbose"`
}

// Level returns the verbosity the flags select. Silent wins over JSON, which
// wins over quiet, which wins over verbose, so contradictory flags err on the
// side of less output; none set means Normal.
func (f Flags) Level() Verbosity {
	if f.JSON && !f.Silent {
		return VerbosityJSON
	}
	return LevelFromFlags(f.Silent, f.Quiet, f.Verbose)
}

// Apply sets the package Level from the flags. Call it once, after the flags
// are parsed and before any output.
func (f Flags) Apply() {
	Level = f.Level()
}

// LevelFromFlags is Flags.Level for tools that read their flags individually,
// for example straight from viper in a cobra pre-run hook. Tools with a JSON
// mode use Flags, which is the only place that level is selected.
func LevelFromFlags(silent, quiet, verbose bool) Verbosity {
	switch {
	case silent:
		return VerbositySilent
	case quiet:
		return VerbosityQuiet
	case verbose:
		return VerbosityVerbose
	default:
		return VerbosityNormal
	}
}

// SetLevelFromFlags sets the package Level from individual flag values; it is
// the one-liner that replaces the switch every tool used to carry.
func SetLevelFromFlags(silent, quiet, verbose bool) {
	Level = LevelFromFlags(silent, quiet, verbose)
}
