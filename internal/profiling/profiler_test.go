package profiling

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfiler(t *testing.T) {
	// Create temporary profile directory
	tmpDir := t.TempDir()

	t.Run("DisabledProfiler", func(t *testing.T) {
		cfg := Config{
			CPUProfile:  false,
			MemProfile:  false,
			ProfileDir:  tmpDir,
			CommandName: "test",
		}

		p := NewProfiler(cfg)
		if p.enabled {
			t.Error("Profiler should be disabled when no profiling is requested")
		}

		// Should not panic
		p.Stop()
		p.Snapshot("test")
		p.LogMetrics("test")
	})

	t.Run("CPUProfileOnly", func(t *testing.T) {
		cfg := Config{
			CPUProfile:  true,
			MemProfile:  false,
			ProfileDir:  tmpDir,
			CommandName: "testcpu",
		}

		p := NewProfiler(cfg)
		if !p.enabled {
			t.Error("Profiler should be enabled")
		}

		// Do some work to generate CPU samples
		doWork(100)

		p.Stop()

		// Check if CPU profile was created
		profiles, err := filepath.Glob(filepath.Join(tmpDir, "testcpu_cpu_*.prof"))
		if err != nil {
			t.Fatalf("Failed to list profiles: %v", err)
		}
		if len(profiles) == 0 {
			t.Error("No CPU profile generated")
		}

		// Verify profile exists and has content
		for _, profile := range profiles {
			info, err := os.Stat(profile)
			if err != nil {
				t.Errorf("Failed to stat profile %s: %v", profile, err)
			}
			if info.Size() == 0 {
				t.Errorf("Profile %s is empty", profile)
			}
		}
	})

	t.Run("MemProfileOnly", func(t *testing.T) {
		cfg := Config{
			CPUProfile:  false,
			MemProfile:  true,
			ProfileDir:  tmpDir,
			CommandName: "testmem",
		}

		p := NewProfiler(cfg)
		if !p.enabled {
			t.Error("Profiler should be enabled")
		}

		// Allocate some memory
		allocateMemory()

		p.Stop()

		// Check if memory profiles were created
		memProfiles, err := filepath.Glob(filepath.Join(tmpDir, "testmem_mem_*.prof"))
		if err != nil {
			t.Fatalf("Failed to list memory profiles: %v", err)
		}
		if len(memProfiles) == 0 {
			t.Error("No memory profile generated")
		}

		allocProfiles, err := filepath.Glob(filepath.Join(tmpDir, "testmem_alloc_*.prof"))
		if err != nil {
			t.Fatalf("Failed to list allocation profiles: %v", err)
		}
		if len(allocProfiles) == 0 {
			t.Error("No allocation profile generated")
		}
	})

	t.Run("BothProfiles", func(t *testing.T) {
		cfg := Config{
			CPUProfile:  true,
			MemProfile:  true,
			ProfileDir:  tmpDir,
			CommandName: "testboth",
		}

		p := NewProfiler(cfg)
		if !p.enabled {
			t.Error("Profiler should be enabled")
		}

		// Do work and allocate memory
		doWork(100)
		allocateMemory()

		p.Stop()

		// Check both profile types
		cpuProfiles, _ := filepath.Glob(filepath.Join(tmpDir, "testboth_cpu_*.prof"))
		memProfiles, _ := filepath.Glob(filepath.Join(tmpDir, "testboth_mem_*.prof"))
		allocProfiles, _ := filepath.Glob(filepath.Join(tmpDir, "testboth_alloc_*.prof"))

		if len(cpuProfiles) == 0 {
			t.Error("No CPU profile generated")
		}
		if len(memProfiles) == 0 {
			t.Error("No memory profile generated")
		}
		if len(allocProfiles) == 0 {
			t.Error("No allocation profile generated")
		}
	})

	t.Run("Snapshot", func(t *testing.T) {
		cfg := Config{
			CPUProfile:  false,
			MemProfile:  true,
			ProfileDir:  tmpDir,
			CommandName: "testsnap",
		}

		p := NewProfiler(cfg)

		// Take snapshots
		p.Snapshot("before")
		allocateMemory()
		p.Snapshot("after")

		p.Stop()

		// Check snapshots
		snapshots, err := filepath.Glob(filepath.Join(tmpDir, "testsnap_snapshot_*.prof"))
		if err != nil {
			t.Fatalf("Failed to list snapshots: %v", err)
		}

		foundBefore := false
		foundAfter := false
		for _, snapshot := range snapshots {
			if strings.Contains(snapshot, "_before_") {
				foundBefore = true
			}
			if strings.Contains(snapshot, "_after_") {
				foundAfter = true
			}
		}

		if !foundBefore {
			t.Error("Before snapshot not found")
		}
		if !foundAfter {
			t.Error("After snapshot not found")
		}
	})
}

func TestGetMetrics(t *testing.T) {
	metrics := GetMetrics()

	// Basic sanity checks
	if metrics.NumCPU <= 0 {
		t.Error("NumCPU should be positive")
	}
	if metrics.NumGoroutine <= 0 {
		t.Error("NumGoroutine should be positive")
	}
	if metrics.Alloc == 0 {
		t.Error("Alloc should not be zero")
	}
}

func TestFlags(t *testing.T) {
	f := Flags{}

	// Test default state
	if f.Enabled() {
		t.Error("Flags should not be enabled by default")
	}

	// Test individual flags
	f.CPUProfile = true
	if !f.Enabled() {
		t.Error("Should be enabled when CPUProfile is true")
	}

	f.CPUProfile = false
	f.MemProfile = true
	if !f.Enabled() {
		t.Error("Should be enabled when MemProfile is true")
	}

	f.MemProfile = false
	f.Profile = true
	if !f.Enabled() {
		t.Error("Should be enabled when Profile is true")
	}

	// Test ToConfig
	cfg := f.ToConfig("testcmd")
	if cfg.CommandName != "testcmd" {
		t.Error("CommandName not set correctly")
	}
	if !cfg.CPUProfile || !cfg.MemProfile {
		t.Error("Profile flag should enable both CPU and memory profiling")
	}
}

// Helper functions for testing

func doWork(iterations int) {
	// CPU-intensive work
	result := 0
	for i := 0; i < iterations*1000; i++ {
		for j := 0; j < 100; j++ {
			result += i * j
		}
	}
	_ = result
}

func allocateMemory() {
	// Allocate some memory
	const numAllocs = 100
	const allocSize = 1024 * 1024 // 1MB

	allocations := make([][]byte, numAllocs)
	for i := 0; i < numAllocs; i++ {
		allocations[i] = make([]byte, allocSize)
		// Touch the memory to ensure it's allocated
		for j := 0; j < allocSize; j += 4096 {
			allocations[i][j] = byte(i)
		}
	}

	runtime.KeepAlive(allocations)
}
