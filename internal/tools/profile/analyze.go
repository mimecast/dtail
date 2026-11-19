package profile

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mimecast/dtail/internal/tools/common"
)

// Info holds information about a profile file.
type Info struct {
	Path    string
	Tool    string
	Type    string // cpu, mem, alloc
	ModTime string
	Size    int64
}

func runAnalyze() error {
	args := flag.Args()
	if len(args) == 0 {
		return fmt.Errorf("no profile file specified")
	}

	profilePath := args[0]
	if !common.FileExists(profilePath) {
		return fmt.Errorf("profile file not found: %s", profilePath)
	}

	// Determine if web mode requested
	for _, arg := range args[1:] {
		if arg == "-web" || arg == "--web" {
			return openWebProfile(profilePath)
		}
	}

	// Default to text analysis
	return analyzeProfile(profilePath)
}

func listProfiles(cfg *Config) error {
	common.PrintSection("Available Profiles")

	profiles, err := findProfiles(cfg.ProfileDir)
	if err != nil {
		return err
	}

	if len(profiles) == 0 {
		fmt.Printf("No profiles found in %s\n", cfg.ProfileDir)
		return nil
	}

	// Group by tool
	byTool := make(map[string][]Info)
	for _, p := range profiles {
		byTool[p.Tool] = append(byTool[p.Tool], p)
	}

	// Sort tools
	var tools []string
	for tool := range byTool {
		tools = append(tools, tool)
	}
	sort.Strings(tools)

	// Display profiles
	for _, tool := range tools {
		fmt.Printf("\n%s profiles:\n", tool)
		toolProfiles := byTool[tool]

		// Sort by modification time (newest first)
		sort.Slice(toolProfiles, func(i, j int) bool {
			return toolProfiles[i].ModTime > toolProfiles[j].ModTime
		})

		for _, p := range toolProfiles {
			fmt.Printf("  %-8s %s  %8s  %s\n",
				p.Type, p.ModTime, common.FormatSize(p.Size), filepath.Base(p.Path))
		}
	}

	fmt.Printf("\nTotal: %d profiles\n", len(profiles))
	fmt.Printf("\nUsage: dtail-tools profile -mode analyze <profile_file>\n")

	return nil
}

func findProfiles(dir string) ([]Info, error) {
	var profiles []Info

	pattern := filepath.Join(dir, "*.prof")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		// Parse filename to extract tool and type
		base := filepath.Base(path)
		parts := strings.Split(base, "_")
		if len(parts) < 3 {
			continue
		}

		tool := parts[0]
		profType := parts[1]

		profiles = append(profiles, Info{
			Path:    path,
			Tool:    tool,
			Type:    profType,
			ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
			Size:    info.Size(),
		})
	}

	return profiles, nil
}

func analyzeProfile(profilePath string) error {
	// Detect profile type
	isMemProfile := strings.Contains(profilePath, "_mem_") || strings.Contains(profilePath, "_alloc_")

	fmt.Printf("Analyzing %s\n", profilePath)
	fmt.Println(strings.Repeat("-", 60))

	// Default analysis
	if err := showTopFunctions(profilePath, 10, isMemProfile); err != nil {
		return err
	}

	// Show tips
	fmt.Println("\nAnalysis tips:")
	if isMemProfile {
		fmt.Println("  - Use -alloc_space to see total allocations")
		fmt.Println("  - Use -alloc_objects to see allocation counts")
		fmt.Println("  - Use -inuse_space to see current memory usage")
	} else {
		fmt.Println("  - Use -cum to sort by cumulative time")
		fmt.Println("  - Use -list <function> to see source code")
		fmt.Println("  - Use -web to open interactive flame graph")
	}

	return nil
}

func showTopFunctions(profilePath string, count int, isMemProfile bool) error {
	args := []string{"tool", "pprof", "-top", fmt.Sprintf("-nodecount=%d", count)}

	if isMemProfile {
		args = append(args, "-alloc_space")
	}

	args = append(args, profilePath)

	cmd := exec.Command("go", args...)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("pprof failed: %w", err)
	}

	// Parse and display output
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	lineCount := 0
	inTop := false

	fmt.Printf("Top %d functions (sorted by flat):\n", count)
	fmt.Println("================================================================")

	for scanner.Scan() {
		line := scanner.Text()

		// Skip header lines
		if strings.HasPrefix(line, "File:") || strings.HasPrefix(line, "Type:") ||
			strings.HasPrefix(line, "Time:") || strings.HasPrefix(line, "Duration:") {
			continue
		}

		// Start printing from the table header
		if strings.Contains(line, "flat") && strings.Contains(line, "cum") {
			inTop = true
			fmt.Println("# Command: go " + strings.Join(args[1:], " "))
		}

		if inTop {
			fmt.Println(line)
			if line != "" {
				lineCount++
			}
			if lineCount > count+2 { // +2 for header and separator
				break
			}
		}
	}

	return nil
}

func openWebProfile(profilePath string) error {
	fmt.Printf("Starting pprof web server for %s...\n", profilePath)
	fmt.Println("Opening http://localhost:8080 in your browser")
	fmt.Println("Press Ctrl+C to stop")

	cmd := exec.Command("go", "tool", "pprof", "-http=:8080", profilePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
