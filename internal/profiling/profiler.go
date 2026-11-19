package profiling

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"
)

// Profiler manages CPU and memory profiling for dtail commands
type Profiler struct {
	cpuProfile  *os.File
	memProfile  string
	profileDir  string
	commandName string
	enabled     bool
}

// Config holds profiling configuration
type Config struct {
	// Enable CPU profiling
	CPUProfile bool
	// Enable memory profiling
	MemProfile bool
	// Directory to store profiles
	ProfileDir string
	// Command name for profile naming
	CommandName string
}

// NewProfiler creates a new profiler instance
func NewProfiler(cfg Config) *Profiler {
	if !cfg.CPUProfile && !cfg.MemProfile {
		return &Profiler{enabled: false}
	}

	p := &Profiler{
		profileDir:  cfg.ProfileDir,
		commandName: cfg.CommandName,
		enabled:     true,
	}

	// Create profile directory if it doesn't exist
	if p.profileDir == "" {
		p.profileDir = "profiles"
	}
	if err := os.MkdirAll(p.profileDir, 0755); err != nil {
		log.Printf("Failed to create profile directory: %v", err)
		p.enabled = false
		return p
	}

	// Start CPU profiling if enabled
	if cfg.CPUProfile {
		p.startCPUProfile()
	}

	// Set memory profile path if enabled
	if cfg.MemProfile {
		timestamp := time.Now().Format("20060102_150405")
		p.memProfile = filepath.Join(p.profileDir, fmt.Sprintf("%s_mem_%s.prof", p.commandName, timestamp))
	}

	return p
}

// startCPUProfile starts CPU profiling
func (p *Profiler) startCPUProfile() {
	timestamp := time.Now().Format("20060102_150405")
	cpuProfilePath := filepath.Join(p.profileDir, fmt.Sprintf("%s_cpu_%s.prof", p.commandName, timestamp))

	f, err := os.Create(cpuProfilePath)
	if err != nil {
		log.Printf("Failed to create CPU profile file: %v", err)
		return
	}

	if err := pprof.StartCPUProfile(f); err != nil {
		log.Printf("Failed to start CPU profile: %v", err)
		if closeErr := f.Close(); closeErr != nil {
			log.Printf("Failed to close unused CPU profile file: %v", closeErr)
		}
		return
	}

	p.cpuProfile = f
	log.Printf("Started CPU profiling: %s", cpuProfilePath)
}

// Stop stops all profiling and writes profiles to disk
func (p *Profiler) Stop() {
	if !p.enabled {
		return
	}

	// Stop CPU profiling
	if p.cpuProfile != nil {
		pprof.StopCPUProfile()
		if err := p.cpuProfile.Close(); err != nil {
			log.Printf("Failed to close CPU profile: %v", err)
		} else {
			log.Printf("Stopped CPU profiling")
		}
		p.cpuProfile = nil
	}

	// Write memory profile
	if p.memProfile != "" {
		p.writeMemProfile()
	}
}

// writeMemProfile writes memory allocation profile to disk
func (p *Profiler) writeMemProfile() {
	f, err := os.Create(p.memProfile)
	if err != nil {
		log.Printf("Failed to create memory profile file: %v", err)
		return
	}

	// Force GC before capturing memory profile for more accurate results
	runtime.GC()

	writeErr := pprof.WriteHeapProfile(f)
	closeErr := f.Close()
	if writeErr != nil {
		log.Printf("Failed to write memory profile: %v", writeErr)
		if closeErr != nil {
			log.Printf("Failed to close memory profile after write failure: %v", closeErr)
		}
		return
	}
	if closeErr != nil {
		log.Printf("Failed to close memory profile: %v", closeErr)
		return
	}

	log.Printf("Wrote memory profile: %s", p.memProfile)

	// Also write allocation profile for detailed allocation tracking
	allocProfilePath := filepath.Join(p.profileDir,
		fmt.Sprintf("%s_alloc_%s.prof", p.commandName, time.Now().Format("20060102_150405")))

	allocFile, err := os.Create(allocProfilePath)
	if err != nil {
		log.Printf("Failed to create allocation profile file: %v", err)
		return
	}

	// Set allocation profiling rate to capture more samples
	runtime.MemProfileRate = 1

	writeErr = pprof.Lookup("allocs").WriteTo(allocFile, 0)
	closeErr = allocFile.Close()
	if writeErr != nil {
		log.Printf("Failed to write allocation profile: %v", writeErr)
		if closeErr != nil {
			log.Printf("Failed to close allocation profile after write failure: %v", closeErr)
		}
		return
	}
	if closeErr != nil {
		log.Printf("Failed to close allocation profile: %v", closeErr)
		return
	}

	log.Printf("Wrote allocation profile: %s", allocProfilePath)
}

// Snapshot takes a memory snapshot at any point during execution
func (p *Profiler) Snapshot(label string) {
	if !p.enabled || p.memProfile == "" {
		return
	}

	timestamp := time.Now().Format("20060102_150405")
	snapshotPath := filepath.Join(p.profileDir,
		fmt.Sprintf("%s_snapshot_%s_%s.prof", p.commandName, label, timestamp))

	f, err := os.Create(snapshotPath)
	if err != nil {
		log.Printf("Failed to create snapshot file: %v", err)
		return
	}
	runtime.GC()
	writeErr := pprof.WriteHeapProfile(f)
	closeErr := f.Close()
	if writeErr != nil {
		log.Printf("Failed to write snapshot: %v", writeErr)
		if closeErr != nil {
			log.Printf("Failed to close snapshot after write failure: %v", closeErr)
		}
		return
	}
	if closeErr != nil {
		log.Printf("Failed to close snapshot: %v", closeErr)
		return
	}

	log.Printf("Wrote memory snapshot: %s (label: %s)", snapshotPath, label)
}

// ProfileMetrics captures and returns current runtime metrics
type ProfileMetrics struct {
	// Memory statistics
	Alloc        uint64    // Bytes allocated and still in use
	TotalAlloc   uint64    // Bytes allocated (even if freed)
	Sys          uint64    // Bytes obtained from system
	NumGC        uint32    // Number of completed GC cycles
	LastGC       time.Time // Time of last GC
	PauseTotalNs uint64    // Total GC pause time in nanoseconds

	// Goroutine count
	NumGoroutine int

	// CPU count
	NumCPU int
}

// GetMetrics returns current runtime metrics
func GetMetrics() ProfileMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return ProfileMetrics{
		Alloc:        m.Alloc,
		TotalAlloc:   m.TotalAlloc,
		Sys:          m.Sys,
		NumGC:        m.NumGC,
		LastGC:       time.Unix(0, int64(m.LastGC)),
		PauseTotalNs: m.PauseTotalNs,
		NumGoroutine: runtime.NumGoroutine(),
		NumCPU:       runtime.NumCPU(),
	}
}

// LogMetrics logs current runtime metrics
func (p *Profiler) LogMetrics(label string) {
	if !p.enabled {
		return
	}

	metrics := GetMetrics()
	log.Printf("Profile metrics [%s]: alloc=%.2fMB total_alloc=%.2fMB sys=%.2fMB num_gc=%d gc_pause=%.2fms goroutines=%d",
		label,
		float64(metrics.Alloc)/1024/1024,
		float64(metrics.TotalAlloc)/1024/1024,
		float64(metrics.Sys)/1024/1024,
		metrics.NumGC,
		float64(metrics.PauseTotalNs)/1e6,
		metrics.NumGoroutine)
}
