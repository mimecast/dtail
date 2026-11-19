package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Example of using the profiling framework to find performance bottlenecks
func main() {
	fmt.Println("DTail Profiling Example")
	fmt.Println("======================")
	fmt.Println()

	// Create test data
	testFile, err := createTestData()
	if err != nil {
		log.Fatalf("Create test data: %v", err)
	}
	csvFile, err := createCSVData()
	if err != nil {
		removeGeneratedFile(testFile)
		log.Fatalf("Create CSV data: %v", err)
	}
	defer removeGeneratedFile(testFile)
	defer removeGeneratedFile(csvFile)

	// Profile dcat
	fmt.Println("1. Profiling dcat...")
	profileDCat(testFile)

	// Profile dgrep
	fmt.Println("\n2. Profiling dgrep...")
	profileDGrep(testFile)

	// Profile dmap
	fmt.Println("\n3. Profiling dmap...")
	profileDMap(csvFile)

	// Analyze results
	fmt.Println("\n4. Analyzing profiles...")
	analyzeProfiles()
}

func removeGeneratedFile(filename string) {
	if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
		log.Printf("Remove generated file %s: %v", filename, err)
	}
}

func createTestData() (filename string, resultErr error) {
	filename = "test_data.log"
	f, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("create test data: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close test data: %w", err))
		}
	}()

	// Generate 100MB of log data
	for i := 0; i < 1000000; i++ {
		timestamp := time.Now().Format("2006-01-02 15:04:05.000")
		level := []string{"INFO", "WARN", "ERROR", "DEBUG"}[i%4]
		if _, err := fmt.Fprintf(f, "[%s] %s - Processing request %d from user%d\n",
			timestamp, level, i, i%1000); err != nil {
			return "", fmt.Errorf("write test data: %w", err)
		}
	}

	return filename, nil
}

func createCSVData() (filename string, resultErr error) {
	filename = "test_data.csv"
	f, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("create CSV data: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close CSV data: %w", err))
		}
	}()

	// Header
	if _, err := fmt.Fprintln(f, "timestamp,user,action,duration,status"); err != nil {
		return "", fmt.Errorf("write CSV header: %w", err)
	}

	// Generate data
	for i := 0; i < 100000; i++ {
		timestamp := time.Now().Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		user := fmt.Sprintf("user%d", i%100)
		action := []string{"login", "query", "update", "logout"}[i%4]
		duration := 100 + i%900
		status := []string{"success", "failure"}[i%2]

		if _, err := fmt.Fprintf(f, "%s,%s,%s,%d,%s\n", timestamp, user, action, duration, status); err != nil {
			return "", fmt.Errorf("write CSV data: %w", err)
		}
	}

	return filename, nil
}

func profileDCat(testFile string) {
	// Run dcat with profiling
	cmd := exec.Command("../dcat",
		"-profile",
		"-profiledir", "profiles",
		"-plain",
		"-cfg", "none",
		testFile)

	start := time.Now()
	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Printf("Output: %s\n", output)
		return
	}

	fmt.Printf("  Completed in %v\n", duration)

	// Find generated profiles
	profiles, err := filepath.Glob("profiles/dcat_*.prof")
	if err != nil {
		fmt.Printf("Error finding dcat profiles: %v\n", err)
		return
	}
	for _, p := range profiles {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Printf("Error reading profile %s: %v\n", p, err)
			continue
		}
		fmt.Printf("  Generated: %s (%d KB)\n", filepath.Base(p), info.Size()/1024)
	}
}

func profileDGrep(testFile string) {
	// Run dgrep with profiling
	cmd := exec.Command("../dgrep",
		"-profile",
		"-profiledir", "profiles",
		"-plain",
		"-cfg", "none",
		"-regex", "ERROR|WARN",
		"-before", "2",
		"-after", "2",
		testFile)

	start := time.Now()
	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Printf("Output: %s\n", output)
		return
	}

	fmt.Printf("  Completed in %v\n", duration)

	// Count matches
	matches := strings.Count(string(output), "ERROR") + strings.Count(string(output), "WARN")
	fmt.Printf("  Found %d matches\n", matches)
}

func profileDMap(csvFile string) {
	// Get absolute path for the CSV file
	absPath, err := filepath.Abs(csvFile)
	if err != nil {
		fmt.Printf("Error getting absolute path: %v\n", err)
		return
	}

	// Run dmap with profiling - correct syntax with -files flag
	queries := []string{
		"select count(*)",
		"select user, count(*) group by user",
		"select action, avg(duration), max(duration) group by action",
	}

	for i, query := range queries {
		fmt.Printf("  Query %d: %s\n", i+1, query)

		cmd := exec.Command("../dmap",
			"-profile",
			"-profiledir", "profiles",
			"-plain",
			"-cfg", "none",
			"-files", absPath,
			"-query", query)

		start := time.Now()
		output, err := cmd.CombinedOutput()
		duration := time.Since(start)

		if err != nil {
			fmt.Printf("    Error: %v\n", err)
			fmt.Printf("    Output: %s\n", output)
			continue
		}

		fmt.Printf("    Completed in %v\n", duration)
	}
}

func truncateQuery(query string) string {
	if len(query) > 50 {
		return query[:47] + "..."
	}
	return query
}

func analyzeProfiles() {
	// Find latest CPU profiles
	cpuProfiles, err := filepath.Glob("profiles/*_cpu_*.prof")
	if err != nil {
		fmt.Printf("Error finding CPU profiles: %v\n", err)
		return
	}
	if len(cpuProfiles) == 0 {
		fmt.Println("No CPU profiles found")
		return
	}

	// Analyze each tool's CPU profile
	tools := []string{"dcat", "dgrep", "dmap"}
	for _, tool := range tools {
		var latestProfile string
		var latestTime time.Time

		// Find latest profile for this tool
		for _, profile := range cpuProfiles {
			if strings.Contains(profile, tool+"_cpu_") {
				info, err := os.Stat(profile)
				if err != nil {
					fmt.Printf("  Error reading profile %s: %v\n", profile, err)
					continue
				}
				if info.ModTime().After(latestTime) {
					latestProfile = profile
					latestTime = info.ModTime()
				}
			}
		}

		if latestProfile == "" {
			continue
		}

		fmt.Printf("\nAnalyzing %s CPU profile:\n", tool)

		// Run dtail-tools profile analyze
		cmd := exec.Command("../dtail-tools",
			"profile", "-mode", "analyze",
			latestProfile)

		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("  Error analyzing: %v\n", err)
			continue
		}

		// Extract and display key information
		lines := strings.Split(string(output), "\n")
		inTable := false
		for _, line := range lines {
			if strings.Contains(line, "Function") && strings.Contains(line, "Flat") {
				inTable = true
			}
			if inTable && (strings.Contains(line, "%") || strings.Contains(line, "---")) {
				fmt.Printf("  %s\n", line)
			}
			if inTable && line == "" {
				break
			}
		}

		// Suggest optimizations based on findings
		suggestOptimizations(tool, string(output))
	}
}

func suggestOptimizations(tool string, analysis string) {
	fmt.Printf("\n  Optimization suggestions for %s:\n", tool)

	// Common patterns to look for
	suggestions := []struct {
		pattern    string
		suggestion string
	}{
		{"regexp.Compile", "  - Pre-compile regex patterns instead of compiling in loops"},
		{"strings.Join", "  - Use strings.Builder for string concatenation"},
		{"runtime.mallocgc", "  - High allocation rate; consider object pooling"},
		{"syscall", "  - I/O bottleneck; consider buffering or async I/O"},
		{"runtime.gcBgMarkWorker", "  - High GC pressure; reduce allocations"},
	}

	foundAny := false
	for _, s := range suggestions {
		if strings.Contains(analysis, s.pattern) {
			fmt.Println(s.suggestion)
			foundAny = true
		}
	}

	if !foundAny {
		fmt.Println("  - Profile looks good; no obvious bottlenecks found")
	}
}

// ExampleBenchmarkWithProfiling demonstrates how to use profiling in tests.
func ExampleBenchmarkWithProfiling() {
	// This would typically be in a _test.go file
	fmt.Print(`
Example benchmark with profiling:

func BenchmarkDCatLargeFile(b *testing.B) {
    // Enable profiling for this specific benchmark
    if *cpuprofile != "" {
        f, _ := os.Create(*cpuprofile)
        pprof.StartCPUProfile(f)
        defer pprof.StopCPUProfile()
    }
    
    // Generate test file
    testFile := generateLargeFile(b)
    defer os.Remove(testFile)
    
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        cmd := exec.Command("./dcat", "-plain", testFile)
        cmd.Run()
    }
    
    if *memprofile != "" {
        f, _ := os.Create(*memprofile)
        runtime.GC()
        pprof.WriteHeapProfile(f)
        f.Close()
    }
}

Run with: go test -bench=BenchmarkDCatLargeFile -cpuprofile=cpu.prof -memprofile=mem.prof
`)
}
