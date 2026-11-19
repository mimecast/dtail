package main

import (
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/tools/benchmark"
	"github.com/mimecast/dtail/internal/tools/pgo"
	"github.com/mimecast/dtail/internal/tools/profile"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	// Remove command from args for subcommand parsing
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)

	switch command {
	case "profile":
		if err := profile.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "benchmark":
		if err := benchmark.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "pgo":
		if err := pgo.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("dtail-tools - DTail performance analysis toolkit")
	fmt.Println()
	fmt.Println("Usage: dtail-tools <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  profile    Run profiling on dtail commands")
	fmt.Println("  benchmark  Run benchmarks and manage baselines")
	fmt.Println("  pgo        Profile-Guided Optimization for dtail commands")
	fmt.Println("  help       Show this help message")
	fmt.Println()
	fmt.Println("Run 'dtail-tools <command> -h' for command-specific help")
}
