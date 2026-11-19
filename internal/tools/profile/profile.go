package profile

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/tools/common"
)

// Config holds profiling configuration
type Config struct {
	Mode        string
	ProfileDir  string
	TestDataDir string
	Runs        int
	NoColor     bool
	Commands    []string
	Timeout     time.Duration
}

// Run executes the profiling command
func Run() error {
	cfg := parseFlags()

	// Create directories
	if err := common.EnsureDirectory(cfg.ProfileDir); err != nil {
		return fmt.Errorf("failed to create profile directory: %w", err)
	}
	if err := common.EnsureDirectory(cfg.TestDataDir); err != nil {
		return fmt.Errorf("failed to create test data directory: %w", err)
	}

	switch cfg.Mode {
	case "quick":
		return runQuickProfile(cfg)
	case "full":
		return runFullProfile(cfg)
	case "dmap":
		return runDMapProfile(cfg)
	case "analyze":
		return runAnalyze()
	case "list":
		return listProfiles(cfg)
	default:
		return fmt.Errorf("unknown profile mode: %s", cfg.Mode)
	}
}

func parseFlags() *Config {
	cfg := &Config{
		Commands: []string{"dcat", "dgrep", "dmap"},
		Timeout:  30 * time.Second,
	}

	flag.StringVar(&cfg.Mode, "mode", "quick", "Profile mode: quick, full, dmap, analyze, list")
	flag.StringVar(&cfg.ProfileDir, "dir", "profiles", "Profile output directory")
	flag.StringVar(&cfg.TestDataDir, "testdata", "testdata", "Test data directory")
	flag.IntVar(&cfg.Runs, "runs", 1, "Number of profiling runs")
	flag.BoolVar(&cfg.NoColor, "nocolor", false, "Disable colored output")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "Timeout for profiling runs")

	// Custom command list
	var cmdList string
	flag.StringVar(&cmdList, "commands", "", "Comma-separated list of commands to profile")

	flag.Parse()

	if cmdList != "" {
		cfg.Commands = strings.Split(cmdList, ",")
	}

	return cfg
}

func runQuickProfile(cfg *Config) error {
	common.PrintSection("DTail Quick Profiling")

	// Generate test data
	gen := common.NewDataGenerator()

	logFile := filepath.Join(cfg.TestDataDir, "quick_test.log")
	csvFile := filepath.Join(cfg.TestDataDir, "quick_test.csv")

	common.PrintInfo("Generating test data...\n")
	if err := gen.GenerateFile(logFile, "10MB", common.FormatLog); err != nil {
		return fmt.Errorf("failed to generate log file: %w", err)
	}
	if err := gen.GenerateFile(csvFile, "10MB", common.FormatCSV); err != nil {
		return fmt.Errorf("failed to generate CSV file: %w", err)
	}

	// Build commands
	common.PrintInfo("Building commands...\n")
	if err := common.BuildCommands("dcat", "dgrep", "dmap"); err != nil {
		return err
	}

	// Profile each command
	common.PrintSection("Running quick profiles...")

	// Profile dcat
	if err := profileCommand("dcat", "dcat",
		[]string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none", logFile},
		cfg.Timeout); err != nil {
		return err
	}

	// Profile dgrep
	if err := profileCommand("dgrep", "dgrep",
		[]string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-regex", "user[0-9]+", logFile},
		cfg.Timeout); err != nil {
		return err
	}

	// Profile dmap
	query := `select count($line),avg($duration) group by $user logformat csv`
	if err := profileCommand("dmap", "dmap",
		[]string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-query", query, "-files", csvFile},
		cfg.Timeout); err != nil {
		return err
	}

	// Analyze results
	return analyzeLatestProfiles(cfg)
}

func runFullProfile(cfg *Config) error {
	common.PrintSection("DTail Full Profiling")

	// Generate test data
	gen := common.NewDataGenerator()

	testFiles := map[string]string{
		"small.log":        "10MB",
		"medium.log":       "100MB",
		"test.csv":         "50MB",
		"dtail_format.log": "100000", // lines
	}

	common.PrintInfo("Generating test data...\n")
	for filename, size := range testFiles {
		fullPath := filepath.Join(cfg.TestDataDir, filename)
		switch {
		case filename == "dtail_format.log":
			lines := 100000
			if err := gen.GenerateLogFileWithLines(fullPath, lines, common.FormatDTail); err != nil {
				return fmt.Errorf("failed to generate %s: %w", filename, err)
			}
		case strings.HasSuffix(filename, ".csv"):
			if err := gen.GenerateFile(fullPath, size, common.FormatCSV); err != nil {
				return fmt.Errorf("failed to generate %s: %w", filename, err)
			}
		default:
			if err := gen.GenerateFile(fullPath, size, common.FormatLog); err != nil {
				return fmt.Errorf("failed to generate %s: %w", filename, err)
			}
		}
	}

	// Build commands
	common.PrintInfo("Building commands...\n")
	if err := common.BuildCommands("dcat", "dgrep", "dmap"); err != nil {
		return err
	}

	// Run profiling
	common.PrintSection("Running full profiling suite...")

	// Profile configurations
	profiles := []struct {
		cmd  string
		name string
		args []string
	}{
		// dcat profiles
		{"dcat", "small_file", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			filepath.Join(cfg.TestDataDir, "small.log")}},
		{"dcat", "medium_file", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			filepath.Join(cfg.TestDataDir, "medium.log")}},

		// dgrep profiles
		{"dgrep", "simple_pattern", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-regex", "ERROR", filepath.Join(cfg.TestDataDir, "medium.log")}},
		{"dgrep", "complex_pattern", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-regex", "(ERROR|WARN).*user[0-9]+", filepath.Join(cfg.TestDataDir, "medium.log")}},

		// dmap profiles
		{"dmap", "simple_count", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-query", "from STATS select count(*)", "-files", filepath.Join(cfg.TestDataDir, "dtail_format.log")}},
		{"dmap", "aggregations", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-query", "from STATS select sum($goroutines),avg($cgocalls),max(lifetimeConnections)",
			"-files", filepath.Join(cfg.TestDataDir, "dtail_format.log")}},
		{"dmap", "csv_query", []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-query", `select count($line),count($user),count($action) group by $user,$action where $status eq "success" logformat csv`,
			"-files", filepath.Join(cfg.TestDataDir, "test.csv")}},
	}

	for _, p := range profiles {
		common.PrintInfo("\nProfiling %s - %s\n", p.cmd, p.name)
		for i := 1; i <= cfg.Runs; i++ {
			if cfg.Runs > 1 {
				fmt.Printf("  Run %d/%d...\n", i, cfg.Runs)
			}
			if err := profileCommand(p.cmd, p.cmd, p.args, cfg.Timeout); err != nil {
				return fmt.Errorf("failed to profile %s-%s: %w", p.cmd, p.name, err)
			}
			if i < cfg.Runs {
				time.Sleep(1 * time.Second) // Small delay between runs
			}
		}
	}

	return analyzeLatestProfiles(cfg)
}

func runDMapProfile(cfg *Config) error {
	common.PrintSection("DTail dmap Profiling")

	// Generate MapReduce test data
	gen := common.NewDataGenerator()

	smallFile := filepath.Join(cfg.TestDataDir, "stats_small.log")
	mediumFile := filepath.Join(cfg.TestDataDir, "stats_medium.log")

	common.PrintInfo("Preparing MapReduce test data...\n")
	if err := gen.GenerateLogFileWithLines(smallFile, 1000, common.FormatDTail); err != nil {
		return fmt.Errorf("failed to generate small file: %w", err)
	}
	if err := gen.GenerateLogFileWithLines(mediumFile, 1000000, common.FormatDTail); err != nil {
		return fmt.Errorf("failed to generate medium file: %w", err)
	}

	// Build dmap
	common.PrintInfo("Building dmap...\n")
	if err := common.BuildCommand("dmap"); err != nil {
		return err
	}

	// Profile different queries
	common.PrintSection("Profiling dmap queries...")

	queries := []struct {
		name  string
		query string
		file  string
	}{
		{"Count by hostname", "from STATS select count($line) group by hostname", smallFile},
		{"Sum and average", "from STATS select sum($goroutines),avg($goroutines) group by hostname", smallFile},
		{"Min and max", "from STATS select min(currentConnections),max(lifetimeConnections) group by hostname", smallFile},
		{"Large file processing", "from STATS select count($line),avg($goroutines) group by hostname", mediumFile},
	}

	for _, q := range queries {
		common.PrintInfo("\nQuery: %s\n", q.name)
		args := []string{"-profile", "-profiledir", cfg.ProfileDir, "-plain", "-cfg", "none",
			"-query", q.query, "-files", q.file}
		if err := profileCommand("dmap", "dmap", args, cfg.Timeout); err != nil {
			return fmt.Errorf("failed to profile query %s: %w", q.name, err)
		}
	}

	return analyzeLatestProfiles(cfg)
}

func profileCommand(name, cmd string, args []string, timeout time.Duration) error {
	fmt.Printf("Command: %s %s\n", cmd, strings.Join(args, " "))

	command := exec.Command("./"+cmd, args...)
	command.Stdout = nil // Suppress output during profiling
	command.Stderr = os.Stderr

	if err := command.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()

	select {
	case <-time.After(timeout):
		timeoutErr := fmt.Errorf("command timed out after %v", timeout)
		if err := command.Process.Kill(); err != nil {
			return errors.Join(timeoutErr, fmt.Errorf("kill timed-out command: %w", err))
		}
		return timeoutErr
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "signal: interrupt") {
			return err
		}
	}

	// Find generated profile
	pattern := filepath.Join(profileDirFromArgs(args), fmt.Sprintf("%s_cpu_*.prof", name))
	matches, err := filesByNewestModTime(pattern)
	if err != nil {
		return fmt.Errorf("find generated profile: %w", err)
	}
	if len(matches) > 0 {
		fmt.Printf("  Generated: %s\n", filepath.Base(matches[0]))
	}

	return nil
}

func profileDirFromArgs(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-profiledir" {
			return args[i+1]
		}
	}
	return "profiles"
}

func analyzeLatestProfiles(cfg *Config) error {
	common.PrintSection("Profile Analysis")

	// Find latest profiles for each command
	for _, cmd := range cfg.Commands {
		cpuPattern := filepath.Join(cfg.ProfileDir, fmt.Sprintf("%s_cpu_*.prof", cmd))
		memPattern := filepath.Join(cfg.ProfileDir, fmt.Sprintf("%s_mem_*.prof", cmd))

		cpuProfiles, err := filesByNewestModTime(cpuPattern)
		if err != nil {
			return fmt.Errorf("find %s CPU profiles: %w", cmd, err)
		}
		memProfiles, err := filesByNewestModTime(memPattern)
		if err != nil {
			return fmt.Errorf("find %s memory profiles: %w", cmd, err)
		}

		if len(cpuProfiles) > 0 {
			fmt.Printf("\n%s CPU Profile: %s\n", cmd, filepath.Base(cpuProfiles[0]))
			if err := showTopFunctions(cpuProfiles[0], 5, false); err != nil {
				fmt.Printf("  Analysis failed: %v\n", err)
			}
		}

		if len(memProfiles) > 0 {
			fmt.Printf("\n%s Memory Profile: %s\n", cmd, filepath.Base(memProfiles[0]))
			if err := showTopFunctions(memProfiles[0], 5, true); err != nil {
				fmt.Printf("  Analysis failed: %v\n", err)
			}
		}
	}

	common.PrintSuccess("\nProfiling complete!\n")
	fmt.Println("\nTo analyze profiles in detail:")
	fmt.Printf("  go tool pprof %s/<profile_file>\n", cfg.ProfileDir)
	fmt.Printf("  dtail-tools profile -mode analyze <profile_file>\n")

	return nil
}

type profileFile struct {
	path    string
	modTime time.Time
}

func filesByNewestModTime(pattern string) ([]string, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	files := make([]profileFile, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", path, err)
		}
		files = append(files, profileFile{path: path, modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	paths = paths[:0]
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths, nil
}
