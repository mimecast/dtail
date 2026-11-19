package profiling

import "flag"

// Flags holds command-line flags for profiling
type Flags struct {
	// Enable CPU profiling
	CPUProfile bool
	// Enable memory profiling
	MemProfile bool
	// Enable all profiling (CPU + memory)
	Profile bool
	// Directory to store profiles
	ProfileDir string
}

// AddFlags adds profiling flags to the flag set
func AddFlags(f *Flags) {
	BindFlags(flag.CommandLine, f)
}

// BindFlags adds profiling flags to fs.
func BindFlags(fs *flag.FlagSet, f *Flags) {
	fs.BoolVar(&f.CPUProfile, "cpuprofile", false, "Enable CPU profiling")
	fs.BoolVar(&f.MemProfile, "memprofile", false, "Enable memory profiling")
	fs.BoolVar(&f.Profile, "profile", false, "Enable all profiling (CPU + memory)")
	fs.StringVar(&f.ProfileDir, "profiledir", "profiles", "Directory to store profiles")
}

// ToConfig converts flags to profiler config
func (f *Flags) ToConfig(commandName string) Config {
	return Config{
		CPUProfile:  f.CPUProfile || f.Profile,
		MemProfile:  f.MemProfile || f.Profile,
		ProfileDir:  f.ProfileDir,
		CommandName: commandName,
	}
}

// Enabled returns true if any profiling is enabled
func (f *Flags) Enabled() bool {
	return f.CPUProfile || f.MemProfile || f.Profile
}
