package benchmarks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ProfileConfig represents profiling configuration
type ProfileConfig struct {
	// Enable CPU profiling
	EnableCPU bool
	// Enable memory profiling
	EnableMem bool
	// Profile directory
	ProfileDir string
	// Number of iterations
	Iterations int
}

// ProfileResult represents the result of a profiling run
type ProfileResult struct {
	Tool         string
	Operation    string
	Duration     time.Duration
	CPUProfile   string
	MemProfile   string
	AllocProfile string
	ExitCode     int
	Error        error
}

// DefaultProfileConfig returns default profiling configuration
func DefaultProfileConfig() ProfileConfig {
	return ProfileConfig{
		EnableCPU:  true,
		EnableMem:  true,
		ProfileDir: "profiles",
		Iterations: 1,
	}
}

// RunProfiledCommand runs a command with profiling enabled
func RunProfiledCommand(b *testing.B, config ProfileConfig, tool string, args ...string) (*ProfileResult, error) {
	// Ensure profile directory exists
	if err := os.MkdirAll(config.ProfileDir, 0755); err != nil {
		return nil, fmt.Errorf("creating profile dir: %w", err)
	}

	// Build command path
	cmdPath := filepath.Join("..", tool)

	// Add profiling flags
	profileArgs := []string{}
	if config.EnableCPU || config.EnableMem {
		profileArgs = append(profileArgs, "-profile")
		profileArgs = append(profileArgs, "-profiledir", config.ProfileDir)
	}

	// Combine all arguments
	profileArgs = append(profileArgs, args...)

	// Create command
	cmd := exec.Command(cmdPath, profileArgs...)

	// Set up output capture
	outputFile := filepath.Join(config.ProfileDir, fmt.Sprintf("%s_output_%s.log",
		tool, time.Now().Format("20060102_150405")))
	output, err := os.Create(outputFile)
	if err != nil {
		return nil, fmt.Errorf("creating output file: %w", err)
	}
	cmd.Stdout = output
	cmd.Stderr = output

	// Record start time
	start := time.Now()

	// Run command
	runErr := cmd.Run()

	// Record duration
	duration := time.Since(start)

	resultErr := errors.Join(
		wrapError("run profiled command", runErr),
		wrapError("close profiled command output", output.Close()),
	)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	result := &ProfileResult{
		Tool:      tool,
		Operation: strings.Join(args, "_"),
		Duration:  duration,
		ExitCode:  exitCode,
		Error:     resultErr,
	}
	if resultErr != nil {
		return result, resultErr
	}

	// Find generated profile files
	timestamp := time.Now().Format("20060102_1504")
	profiles, err := filepath.Glob(filepath.Join(config.ProfileDir,
		fmt.Sprintf("%s_*_%s*.prof", tool, timestamp)))
	if err != nil {
		return result, fmt.Errorf("find generated profiles: %w", err)
	}

	for _, profile := range profiles {
		switch {
		case strings.Contains(profile, "_cpu_"):
			result.CPUProfile = profile
		case strings.Contains(profile, "_mem_"):
			result.MemProfile = profile
		case strings.Contains(profile, "_alloc_"):
			result.AllocProfile = profile
		}
	}

	return result, nil
}

// ProfileBenchmark runs a benchmark with profiling enabled
func ProfileBenchmark(b *testing.B, name string, tool string, args ...string) {
	config := DefaultProfileConfig()

	b.Run(name+"_profiled", func(b *testing.B) {
		// Generate test data if needed
		testFile := ""
		if tool == "dcat" || tool == "dgrep" {
			testConfig := TestDataConfig{
				Size:          Medium,
				Format:        SimpleLogFormat,
				Compression:   NoCompression,
				LineVariation: 50,
			}
			testFile = GenerateTestFile(b, testConfig)
			cleanupBenchmarkFile(b, testFile)

			// Replace placeholder in args
			for i, arg := range args {
				if arg == "__TESTFILE__" {
					args[i] = testFile
				}
			}
		}

		// Run profiled command
		result, err := RunProfiledCommand(b, config, tool, args...)
		if err != nil {
			b.Fatalf("Command failed: %v", err)
		}

		// Report results
		b.Logf("Profile run completed in %v", result.Duration)
		if result.CPUProfile != "" {
			b.Logf("CPU profile: %s", result.CPUProfile)
		}
		if result.MemProfile != "" {
			b.Logf("Memory profile: %s", result.MemProfile)
		}
		if result.AllocProfile != "" {
			b.Logf("Allocation profile: %s", result.AllocProfile)
		}

		// Analyze profiles using dtail-tools
		dtailToolsPath := filepath.Join("..", "dtail-tools")
		if _, err := os.Stat(dtailToolsPath); err == nil {
			if result.CPUProfile != "" {
				analyzeProfileWithTools(b, dtailToolsPath, result.CPUProfile, "CPU")
			}
			if result.MemProfile != "" {
				analyzeProfileWithTools(b, dtailToolsPath, result.MemProfile, "Memory")
			}
		}
	})
}

// analyzeProfileWithTools runs dtail-tools profile analyze on a profile file
func analyzeProfileWithTools(b *testing.B, dtailToolsPath, profilePath, profileType string) {
	b.Logf("\n%s Profile Analysis:", profileType)

	cmd := exec.Command(dtailToolsPath, "profile", "-mode", "analyze", profilePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		b.Logf("Failed to analyze profile: %v", err)
		return
	}

	// Print analysis output
	lines := strings.Split(string(output), "\n")
	// Print first 10 lines of analysis
	for i, line := range lines {
		if i < 10 && line != "" {
			b.Log(line)
		}
	}
}

// BenchmarkDCatWithProfiling runs dcat with profiling enabled.
func BenchmarkDCatWithProfiling(b *testing.B) {
	ProfileBenchmark(b, "Simple", "dcat", "--plain", "--cfg", "none", "__TESTFILE__")
}

// BenchmarkDGrepWithProfiling runs dgrep with profiling enabled.
func BenchmarkDGrepWithProfiling(b *testing.B) {
	ProfileBenchmark(b, "Regex", "dgrep", "--plain", "--cfg", "none",
		"-regex", "error|warning", "__TESTFILE__")
}

// BenchmarkDMapWithProfiling runs dmap with profiling enabled.
func BenchmarkDMapWithProfiling(b *testing.B) {
	// First generate a CSV file for dmap
	csvFile := filepath.Join(os.TempDir(), "dmap_test.csv")
	generateCSVTestData(b, csvFile, 10000)
	cleanupBenchmarkFile(b, csvFile)

	ProfileBenchmark(b, "Count", "dmap", "--plain", "--cfg", "none",
		"-query", fmt.Sprintf("select count(*) from %s", csvFile))
}

// generateCSVTestData generates CSV test data for dmap
func generateCSVTestData(b *testing.B, filename string, rows int) {
	f, err := os.Create(filename)
	if err != nil {
		b.Fatalf("Failed to create CSV file: %v", err)
	}
	// Write header
	if _, err := fmt.Fprintln(f, "timestamp,user,action,duration"); err != nil {
		b.Fatalf("Failed to write CSV header: %v", errors.Join(err,
			wrapError("close incomplete CSV file", f.Close())))
	}

	// Write data
	for i := 0; i < rows; i++ {
		timestamp := time.Now().Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		user := fmt.Sprintf("user%d", i%100)
		action := []string{"login", "query", "logout"}[i%3]
		duration := 100 + i%500

		if _, err := fmt.Fprintf(f, "%s,%s,%s,%d\n", timestamp, user, action, duration); err != nil {
			b.Fatalf("Failed to write CSV row: %v", errors.Join(err,
				wrapError("close incomplete CSV file", f.Close())))
		}
	}
	if err := f.Close(); err != nil {
		b.Fatalf("Failed to close CSV file: %v", err)
	}
}
