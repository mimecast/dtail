package pgo

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/tools/common"

	"golang.org/x/crypto/ssh"
)

// Config holds PGO configuration
type Config struct {
	Command        string   // Command to build with PGO (dtail, dcat, etc.)
	ProfileDir     string   // Directory containing profile data
	OutputDir      string   // Directory for PGO-optimized binaries
	TestDataSize   int      // Size of test data for profile generation
	TestIterations int      // Number of iterations for profile generation
	Verbose        bool     // Verbose output
	Commands       []string // Specific commands to optimize (empty = all)
	ProfileOnly    bool     // Only generate profiles, don't build optimized binaries
}

// Run executes the PGO workflow
func Run() error {
	var cfg Config

	// Define flags
	flag.StringVar(&cfg.ProfileDir, "profiledir", "pgo-profiles", "Directory for profile data")
	flag.StringVar(&cfg.OutputDir, "outdir", "pgo-build", "Directory for PGO-optimized binaries")
	flag.IntVar(&cfg.TestDataSize, "datasize", 1000000, "Lines of test data for profile generation")
	flag.IntVar(&cfg.TestIterations, "iterations", 3, "Number of profile generation iterations")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "Verbose output")
	flag.BoolVar(&cfg.Verbose, "v", false, "Verbose output (short)")
	flag.BoolVar(&cfg.ProfileOnly, "profileonly", false, "Only generate profiles, don't build optimized binaries")

	// Custom usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: dtail-tools pgo [options] [commands...]\n\n")
		fmt.Fprintf(os.Stderr, "Profile-Guided Optimization (PGO) for DTail commands\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nCommands:\n")
		fmt.Fprintf(os.Stderr, "  If no commands specified, all dtail commands will be optimized\n")
		fmt.Fprintf(os.Stderr, "  Available: dtail, dcat, dgrep, dmap, dserver\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  dtail-tools pgo                    # Optimize all commands\n")
		fmt.Fprintf(os.Stderr, "  dtail-tools pgo dcat dgrep         # Optimize specific commands\n")
		fmt.Fprintf(os.Stderr, "  dtail-tools pgo -v -iterations 5   # Verbose with 5 iterations\n")
	}

	flag.Parse()

	// Get commands from remaining args
	cfg.Commands = flag.Args()
	if len(cfg.Commands) == 0 {
		// All commands can now be profiled to completion. dtail was previously
		// excluded because its follow client never returned from client.Start on
		// -shutdownAfter/SIGINT/SIGTERM, so it never flushed a CPU profile (that
		// is how the committed dtail.pprof came to be 0 bytes). That follow
		// shutdown is now honoured (task 1v0): runDtailWorkload profiles a real
		// follow session bounded by -shutdownAfter, which returns and flushes.
		// dtail (and dserver) run after the always-serverless dcat/dgrep/dmap so
		// that a setup problem in the SSH-based workloads cannot abort the run
		// before the serverless profiles are captured.
		cfg.Commands = []string{"dcat", "dgrep", "dmap", "dtail", "dserver"}
	}

	return runPGO(&cfg)
}

func runPGO(cfg *Config) error {
	// Create directories
	if err := os.MkdirAll(cfg.ProfileDir, 0755); err != nil {
		return fmt.Errorf("creating profile directory: %w", err)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	fmt.Println("DTail Profile-Guided Optimization")
	fmt.Println("=================================")
	fmt.Printf("Commands: %s\n", strings.Join(cfg.Commands, ", "))
	fmt.Printf("Profile directory: %s\n", cfg.ProfileDir)
	fmt.Printf("Output directory: %s\n", cfg.OutputDir)
	fmt.Printf("Test data size: %d lines\n", cfg.TestDataSize)
	fmt.Printf("Iterations: %d\n\n", cfg.TestIterations)

	// Step 1: Build baseline binaries
	fmt.Println("Step 1: Building baseline binaries...")
	if err := buildBaseline(cfg); err != nil {
		return fmt.Errorf("building baseline: %w", err)
	}

	// Step 2: Generate profiles
	fmt.Println("\nStep 2: Generating profiles...")
	if err := generateProfiles(cfg); err != nil {
		return fmt.Errorf("generating profiles: %w", err)
	}

	// If profile-only mode, stop here
	if cfg.ProfileOnly {
		fmt.Println("\nProfile generation complete!")
		fmt.Printf("Profiles saved in: %s\n", cfg.ProfileDir)
		return nil
	}

	// Step 3: Build PGO-optimized binaries
	fmt.Println("\nStep 3: Building PGO-optimized binaries...")
	if err := buildWithPGO(cfg); err != nil {
		return fmt.Errorf("building with PGO: %w", err)
	}

	// Step 4: Compare performance
	fmt.Println("\nStep 4: Comparing performance...")
	if err := comparePerformance(cfg); err != nil {
		return fmt.Errorf("comparing performance: %w", err)
	}

	fmt.Println("\nPGO optimization complete!")
	fmt.Printf("Optimized binaries are in: %s\n", cfg.OutputDir)

	return nil
}

func buildBaseline(cfg *Config) error {
	for _, cmd := range cfg.Commands {
		if cfg.Verbose {
			fmt.Printf("Building %s...\n", cmd)
		}

		// Build command
		buildCmd := exec.Command("go", "build",
			"-o", filepath.Join(cfg.OutputDir, cmd+"-baseline"),
			fmt.Sprintf("./cmd/%s", cmd))

		if cfg.Verbose {
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
		}

		if err := buildCmd.Run(); err != nil {
			return fmt.Errorf("building %s: %w", cmd, err)
		}
	}

	return nil
}

func generateProfiles(cfg *Config) error {
	// Generate test data
	testFiles, err := generateTestData(cfg)
	if err != nil {
		return fmt.Errorf("generating test data: %w", err)
	}
	defer cleanupTestData(testFiles)

	// Run each command to generate profiles
	for _, cmd := range cfg.Commands {
		fmt.Printf("\nGenerating profile for %s...\n", cmd)

		profilePath := filepath.Join(cfg.ProfileDir, fmt.Sprintf("%s.pprof", cmd))

		// Run iterations to collect profile data
		if err := runProfileWorkload(cfg, cmd, testFiles, profilePath); err != nil {
			return fmt.Errorf("running workload for %s: %w", cmd, err)
		}

		// Sanity-check the freshly captured profile. A zero-sample or empty
		// profile is worthless for PGO and must never be silently accepted.
		if err := verifyProfileNonEmpty(cmd, profilePath); err != nil {
			return fmt.Errorf("verifying profile for %s: %w", cmd, err)
		}
	}

	return nil
}

// countRawSamples counts the sample rows in the textual output of
// "go tool pprof -raw". That output lists each captured sample on its own
// indented line between the "Samples:" header (which is followed by a single
// units line, e.g. "samples/count cpu/nanoseconds") and the "Locations"
// section:
//
//	Samples:
//	samples/count cpu/nanoseconds
//	          1   10000000: 1 2 3 4 5 6 7 8
//	          3   30000000: 9 10 11 5 6 7 8
//	Locations
//
// A profile captured from an idle process has the header but no data rows.
// Parsing the textual form keeps this dependency-free (go.mod is intentionally
// lean) and mirrors the existing use of "go tool pprof" for merging.
func countRawSamples(raw string) int {
	inSamples := false
	sawUnits := false
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inSamples {
			if trimmed == "Samples:" {
				inSamples = true
			}
			continue
		}
		// The "Locations" line terminates the sample table.
		if strings.HasPrefix(trimmed, "Locations") {
			break
		}
		if trimmed == "" {
			continue
		}
		// The first non-empty line after the header names the sample units and
		// is not a data row.
		if !sawUnits {
			sawUnits = true
			continue
		}
		// A data row begins with the sample count (a digit) and contains a
		// colon separating the values from the location IDs.
		if strings.ContainsRune(trimmed, ':') && trimmed[0] >= '0' && trimmed[0] <= '9' {
			count++
		}
	}
	return count
}

// profileSampleCount returns the number of CPU samples recorded in the pprof
// file at path by invoking "go tool pprof -raw". It is the mechanism behind the
// zero-sample sanity check.
func profileSampleCount(path string) (int, error) {
	out, err := exec.Command("go", "tool", "pprof", "-raw", path).Output()
	if err != nil {
		return 0, fmt.Errorf("running go tool pprof -raw on %s: %w", path, err)
	}
	return countRawSamples(string(out)), nil
}

// verifyProfileNonEmpty fails when the captured profile for command is missing,
// zero bytes, or contains zero samples. Historically a zero-sample dserver
// capture (an idle server) and a 0-byte dtail capture (an I/O-bound follow)
// both slipped through and were committed, leaving the two most important
// server-mode binaries with no usable PGO despite documented gains. Failing
// loudly here prevents that class of silent-empty-profile regression.
func verifyProfileNonEmpty(command, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("profile not found at %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("profile %s is empty (0 bytes)", path)
	}
	samples, err := profileSampleCount(path)
	if err != nil {
		return err
	}
	if samples == 0 {
		return fmt.Errorf("profile %s has zero samples: the workload did not "+
			"exercise %s under real load", path, command)
	}
	fmt.Printf("  Verified %s profile: %d samples, %d bytes\n", command, samples, info.Size())
	return nil
}

func runProfileWorkload(cfg *Config, command string, testFiles map[string]string, profilePath string) error {
	// Use the baseline binary that was already built
	binary := filepath.Join(cfg.OutputDir, command+"-baseline")
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("baseline binary not found: %s", binary)
	}

	// Merge profiles from multiple runs
	var profiles []string

	for i := 0; i < cfg.TestIterations; i++ {
		if cfg.Verbose {
			fmt.Printf("  Iteration %d/%d...\n", i+1, cfg.TestIterations)
		}

		iterProfile := fmt.Sprintf("%s.%d.pprof", profilePath, i)
		if err := runSingleWorkload(cfg, command, binary, testFiles, iterProfile); err != nil {
			return fmt.Errorf("iteration %d: %w", i+1, err)
		}
		profiles = append(profiles, iterProfile)
	}

	// Merge profiles
	if err := mergeProfiles(profiles, profilePath); err != nil {
		return fmt.Errorf("merging profiles: %w", err)
	}

	// Clean up iteration profiles
	for _, p := range profiles {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("Unable to remove iteration profile %s: %v", p, err)
		}
	}

	return nil
}

func runSingleWorkload(cfg *Config, command, binary string, testFiles map[string]string, profilePath string) error {
	var cmd *exec.Cmd

	// Use a unique profile directory for this iteration
	iterProfileDir := filepath.Join(cfg.ProfileDir, fmt.Sprintf("iter_%s_%d", command, time.Now().UnixNano()))
	if err := os.MkdirAll(iterProfileDir, 0755); err != nil {
		return fmt.Errorf("creating iteration profile dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(iterProfileDir); err != nil {
			log.Printf("Unable to remove iteration profile directory %s: %v", iterProfileDir, err)
		}
	}()

	// Always show what command is being executed
	fmt.Printf("  Executing %s workload...\n", command)

	switch command {
	case "dtail":
		// dtail is a follow client, so unlike the one-shot dcat/dgrep/dmap
		// commands it needs a live server to connect to and a file that keeps
		// growing during the capture. runDtailWorkload owns that lifecycle and
		// bounds the session with -shutdownAfter so the client returns from
		// client.Start and flushes its CPU profile (task 1v0).
		return runDtailWorkload(cfg, binary, iterProfileDir, profilePath)

	case "dcat":
		cmd = exec.Command(binary,
			"-cfg", "none",
			"-plain",
			"-profile",
			"-profiledir", iterProfileDir,
			testFiles["log"])
		fmt.Printf("    Command: %s %s\n", binary, strings.Join(cmd.Args[1:], " "))

	case "dgrep":
		cmd = exec.Command(binary,
			"-cfg", "none",
			"-plain",
			"-profile",
			"-profiledir", iterProfileDir,
			"-regex", "ERROR|WARN",
			testFiles["log"])
		fmt.Printf("    Command: %s %s\n", binary, strings.Join(cmd.Args[1:], " "))

	case "dmap":
		cmd = exec.Command(binary,
			"-cfg", "none",
			"-plain",
			"-profile",
			"-profiledir", iterProfileDir,
			"-files", testFiles["csv"],
			"-query", "select status, count(*) group by status")
		fmt.Printf("    Command: %s %s\n", binary, strings.Join(cmd.Args[1:], " "))

	case "dserver":
		// For dserver, we drive real authenticated client traffic through it.
		// iterProfileDir is threaded through so the workload can stand up an
		// isolated workdir with deterministic key-based auth under it, exactly
		// like the dtail workload does.
		return runDServerWorkload(cfg, binary, iterProfileDir, testFiles, profilePath)

	default:
		return fmt.Errorf("unknown command: %s", command)
	}

	// Capture stderr for debugging
	if cfg.Verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}

	// Run command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", command, err)
	}

	// Find the generated CPU profile
	generatedProfile := filepath.Join(iterProfileDir, fmt.Sprintf("%s_cpu_*.prof", command))
	matches, err := filepath.Glob(generatedProfile)
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("no CPU profile generated (looked for %s)", generatedProfile)
	}

	// Use the first match
	return copyFile(matches[0], profilePath)
}

// copyFile copies src to dst
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dstFile, srcFile)
	closeErr := dstFile.Close()
	return errors.Join(
		wrapFileError("copy", dst, copyErr),
		wrapFileError("close", dst, closeErr),
	)
}

func wrapFileError(operation, path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s file %q: %w", operation, path, err)
}

// dserver profiling parameters. The capture window must overlap sustained
// client traffic; see runDServerWorkload for why the previous fixed-window,
// fire-once approach captured an idle server.
const (
	dserverSSHPort     = 12222
	dserverPProfAddr   = "localhost:16060"
	dserverCaptureSecs = 8
)

// dtail follow workload parameters. Unlike the serverless dcat/dgrep/dmap
// profiles, dtail MUST complete a live SSH handshake before it can stream, so
// the workload stands up a real dserver, sets up deterministic key-based auth
// and keeps a file growing for the whole window. -shutdownAfter bounds the
// session so the client returns from client.Start and flushes its CPU profile
// (task 1v0). The client runs its production default read path so the
// profile reflects the real streaming path rather than the test path.
const (
	dtailSSHPort        = 12223
	dtailShutdownAfterS = 6
	dtailAppendInterval = 50 * time.Millisecond
	// A fixed synthetic user keeps the workload independent of the host's OS
	// user and ~/.ssh setup. Under "-cfg none" every user inherits the default
	// "^/.*" read permission, so this user both authenticates (via the keypair
	// below) and is allowed to read the absolute follow-file path.
	dtailWorkloadUser = "pgoprofile"
)

// runDtailWorkload profiles the dtail follow client end to end. dtail is a
// long-lived follow client and, unlike the serverless one-shot dcat/dgrep/dmap
// commands, it can only produce a representative profile after a live SSH
// handshake, so it needs a running server, working auth and a file that keeps
// growing during the capture. The session is bounded with -shutdownAfter: once
// that deadline cancels the client context the follow reconnect/read loops
// return, client.Start returns and the CPU profile is flushed. Before task 1v0
// the follow client never returned and this capture would have hung, which is
// why dtail used to be excluded from PGO.
func runDtailWorkload(cfg *Config, binary, iterProfileDir, profilePath string) error {
	fmt.Printf("  Executing dtail workload...\n")

	// Prepare an isolated working directory with deterministic key-based auth.
	// The server resolves ./cache/<user>.authorized_keys and ./cache/ssh_host_key
	// relative to its working directory, so everything lives under absWorkDir.
	absWorkDir, workDirErr := filepath.Abs(filepath.Join(iterProfileDir, "dtailwork"))
	if workDirErr != nil {
		return fmt.Errorf("resolving dtail work dir: %w", workDirErr)
	}
	cacheDir := filepath.Join(absWorkDir, "cache")
	if mkdirErr := os.MkdirAll(cacheDir, 0755); mkdirErr != nil {
		return fmt.Errorf("creating dtail work dir: %w", mkdirErr)
	}
	privateKeyPath := filepath.Join(absWorkDir, "id_rsa")
	authorizedKeysPath := filepath.Join(cacheDir, dtailWorkloadUser+".authorized_keys")
	if keyErr := writeDtailAuthKeypair(privateKeyPath, authorizedKeysPath); keyErr != nil {
		return keyErr
	}

	dserverBinary, binaryErr := filepath.Abs(filepath.Join(cfg.OutputDir, "dserver-baseline"))
	if binaryErr != nil {
		return fmt.Errorf("resolving dserver binary path: %w", binaryErr)
	}
	serverCmd := exec.Command(dserverBinary,
		"-cfg", "none",
		"-port", fmt.Sprintf("%d", dtailSSHPort))
	// Run the server inside the prepared workdir so its ./cache lookups resolve
	// to the authorized_keys and host key set up above.
	serverCmd.Dir = absWorkDir
	fmt.Printf("    Starting dserver (dir %s): %s %s\n", absWorkDir, dserverBinary,
		strings.Join(serverCmd.Args[1:], " "))
	if cfg.Verbose {
		serverCmd.Stdout = os.Stdout
		serverCmd.Stderr = os.Stderr
	}
	if startErr := serverCmd.Start(); startErr != nil {
		return fmt.Errorf("starting dserver for dtail workload: %w", startErr)
	}
	defer stopServer(serverCmd)

	if readyErr := waitForServerReady(dtailSSHPort); readyErr != nil {
		return readyErr
	}

	// Create the followed file and keep appending to it in the background so the
	// follow session streams real work for the whole capture window.
	followFile := filepath.Join(absWorkDir, "dtail_follow.log")
	stopAppend, appendWg, appendErr := startFollowFileAppender(followFile)
	if appendErr != nil {
		return appendErr
	}
	defer func() {
		close(stopAppend)
		appendWg.Wait()
	}()

	absProfileDir, profileDirErr := filepath.Abs(iterProfileDir)
	if profileDirErr != nil {
		return fmt.Errorf("resolving dtail profile dir: %w", profileDirErr)
	}
	server := fmt.Sprintf("localhost:%d", dtailSSHPort)
	cmd := exec.Command(binary,
		"-cfg", "none",
		"-plain",
		"-trustAllHosts",
		"-user", dtailWorkloadUser,
		"-auth-key-path", privateKeyPath,
		"-profile",
		"-profiledir", absProfileDir,
		"-servers", server,
		"-files", followFile,
		"-regex", "ERROR",
		"-shutdownAfter", fmt.Sprintf("%d", dtailShutdownAfterS))
	fmt.Printf("    Command: %s %s\n", binary, strings.Join(cmd.Args[1:], " "))

	// Capture client output so a broken handshake (which would yield a useless
	// handshake-churn profile) can be detected after the run.
	var clientOutput bytes.Buffer
	if cfg.Verbose {
		cmd.Stdout = io.MultiWriter(os.Stdout, &clientOutput)
		cmd.Stderr = io.MultiWriter(os.Stderr, &clientOutput)
	} else {
		cmd.Stdout = &clientOutput
		cmd.Stderr = &clientOutput
	}

	if runErr := cmd.Run(); runErr != nil {
		return fmt.Errorf("running dtail follow workload: %w\n%s", runErr, clientOutput.String())
	}

	// Representativeness guard: if the client could not complete the SSH
	// handshake it fell back to reconnect churn and the resulting profile is
	// dominated by crypto/handshake frames (rsa.Sign, clientHandshake) rather
	// than the streaming path we care about. The zero-sample check in
	// verifyProfileNonEmpty cannot distinguish a 1-3 sample churn profile from a
	// real one, so fail loudly here instead of emitting that garbage.
	if marker := detectHandshakeFailure(clientOutput.String()); marker != "" {
		return fmt.Errorf("dtail follow workload did not authenticate (saw %q): the captured "+
			"profile would be SSH-handshake churn, not the streaming path; check the "+
			"workload key setup", marker)
	}

	// dtail writes dtail_cpu_<timestamp>.prof into the profile dir, same as the
	// one-shot commands; locate and copy it into place.
	generatedProfile := filepath.Join(absProfileDir, "dtail_cpu_*.prof")
	matches, globErr := filepath.Glob(generatedProfile)
	if globErr != nil || len(matches) == 0 {
		return fmt.Errorf("no CPU profile generated (looked for %s)", generatedProfile)
	}
	return copyFile(matches[0], profilePath)
}

// writeDtailAuthKeypair generates a throwaway RSA keypair for the dtail
// workload, writing the private key (and a .pub sibling for AUTHKEY
// fast-reconnect registration) to privateKeyPath and the matching authorized
// key line to authorizedKeysPath, which the ephemeral "-cfg none" server reads
// as <user>.authorized_keys. This gives the follow client deterministic
// key-based auth so the capture reflects streaming rather than handshake churn.
func writeDtailAuthKeypair(privateKeyPath, authorizedKeysPath string) error {
	privateKey, keyErr := rsa.GenerateKey(rand.Reader, 2048)
	if keyErr != nil {
		return fmt.Errorf("generating dtail workload key: %w", keyErr)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	if writeErr := os.WriteFile(privateKeyPath, privatePEM, 0600); writeErr != nil {
		return fmt.Errorf("writing dtail workload private key: %w", writeErr)
	}

	publicKey, publicKeyErr := ssh.NewPublicKey(&privateKey.PublicKey)
	if publicKeyErr != nil {
		return fmt.Errorf("deriving dtail workload public key: %w", publicKeyErr)
	}
	authorizedKey := ssh.MarshalAuthorizedKey(publicKey)
	if writeErr := os.WriteFile(privateKeyPath+".pub", authorizedKey, 0644); writeErr != nil {
		return fmt.Errorf("writing dtail workload public key: %w", writeErr)
	}
	if writeErr := os.WriteFile(authorizedKeysPath, authorizedKey, 0600); writeErr != nil {
		return fmt.Errorf("writing dtail workload authorized_keys: %w", writeErr)
	}
	return nil
}

// detectHandshakeFailure returns the first SSH-handshake-failure marker found in
// the dtail client output, or "" when the handshake succeeded. It is the signal
// behind runDtailWorkload's representativeness guard: a failed handshake means
// the profile is reconnect/crypto churn rather than the streaming path.
func detectHandshakeFailure(output string) string {
	for _, marker := range []string{
		"SSH handshake failed",
		"unable to authenticate",
		"no supported methods remain",
		"Unable to find private SSH key",
	} {
		if strings.Contains(output, marker) {
			return marker
		}
	}
	return ""
}

// startFollowFileAppender creates followFile and spawns a goroutine that keeps
// appending matching lines to it until the returned channel is closed. The
// caller closes the channel and waits on the WaitGroup to stop and drain the
// appender.
func startFollowFileAppender(followFile string) (chan struct{}, *sync.WaitGroup, error) {
	fd, err := os.Create(followFile)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dtail follow file: %w", err)
	}

	stopAppend := make(chan struct{})
	var appendWg sync.WaitGroup
	appendWg.Add(1)
	go func() {
		defer appendWg.Done()
		defer func() {
			if err := fd.Close(); err != nil {
				log.Printf("Unable to close dtail follow workload file %s: %v", followFile, err)
			}
		}()
		ticker := time.NewTicker(dtailAppendInterval)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-stopAppend:
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(fd, "%s Hello line %d ERROR test\n",
					time.Now().Format(time.RFC3339Nano), i); err != nil {
					log.Printf("Unable to append dtail follow workload file %s: %v", followFile, err)
					return
				}
			}
		}
	}()

	return stopAppend, &appendWg, nil
}

// runDServerWorkload profiles dserver while real, authenticated client traffic
// flows through it. The server is CPU-profiled via its /debug/pprof HTTP
// endpoint over a fixed window, and a background load generator keeps issuing
// dcat/dgrep/dmap requests for the whole window so the capture records
// server-side streaming/read work.
//
// Deterministic key-based auth (mirrors runDtailWorkload): the workload stands
// up an isolated workdir with a throwaway RSA keypair whose public key is
// written to ./cache/<user>.authorized_keys, runs the server with
// serverCmd.Dir = absWorkDir so that lookup resolves, and points every load
// client at "-user <user> -auth-key-path <privateKey>". Without this, on a bare
// host (no localhost ~/.ssh/authorized_keys) every load client failed SSH auth,
// the server churned through the asymmetric SSH handshake, and the captured
// profile was ~87% crypto (rsa.Sign / bigmod / tls) rather than the streaming
// path we optimize for (found in the 1v0 review). Because the dserver capture
// is an HTTP /debug/pprof fetch that ALWAYS returns a file, that garbage shipped
// silently -- unlike dtail, whose client Run() surfaces the auth error.
//
// Two representativeness guards therefore backstop the auth setup:
//  1. An upfront synchronous probe client: if it cannot complete the SSH
//     handshake we fail before opening the capture window (detectHandshakeFailure).
//  2. A post-capture profile-frame guard: the captured profile must be
//     dominated by streaming/read work, not asymmetric-handshake crypto
//     (verifyDServerProfileRepresentative).
//
// Note on the earlier bug: before the plural "-servers"/"--trustAllHosts"/
// "-files" fix and the sustained-load capture window, clients also aborted on a
// non-existent "-server" flag and the fixed window opened after they finished;
// that left the server idle. Those shape fixes remain; this change adds the
// missing auth and the guards.
func runDServerWorkload(cfg *Config, binary, iterProfileDir string,
	testFiles map[string]string, profilePath string) error {
	fmt.Printf("  Executing dserver workload...\n")

	// Prepare an isolated working directory with deterministic key-based auth.
	// The server resolves ./cache/<user>.authorized_keys and ./cache/ssh_host_key
	// relative to its working directory, so everything lives under absWorkDir.
	absWorkDir, workDirErr := filepath.Abs(filepath.Join(iterProfileDir, "dserverwork"))
	if workDirErr != nil {
		return fmt.Errorf("resolving dserver work dir: %w", workDirErr)
	}
	cacheDir := filepath.Join(absWorkDir, "cache")
	if mkdirErr := os.MkdirAll(cacheDir, 0755); mkdirErr != nil {
		return fmt.Errorf("creating dserver work dir: %w", mkdirErr)
	}
	privateKeyPath := filepath.Join(absWorkDir, "id_rsa")
	authorizedKeysPath := filepath.Join(cacheDir, dtailWorkloadUser+".authorized_keys")
	if keyErr := writeDtailAuthKeypair(privateKeyPath, authorizedKeysPath); keyErr != nil {
		return keyErr
	}

	// The load clients read files by the path they send to the server, and the
	// server (with Dir set to absWorkDir) resolves relative paths against that
	// workdir. Absolute paths both resolve correctly and satisfy the default
	// "^/.*" read permission that "-cfg none" grants every user.
	absTestFiles := make(map[string]string, len(testFiles))
	for k, v := range testFiles {
		abs, absErr := filepath.Abs(v)
		if absErr != nil {
			return fmt.Errorf("resolving test file %q: %w", k, absErr)
		}
		absTestFiles[k] = abs
	}

	// The server binary must be absolute because serverCmd.Dir is set below;
	// cfg.OutputDir is otherwise a relative path resolved against the tool cwd.
	absBinary, binaryErr := filepath.Abs(binary)
	if binaryErr != nil {
		return fmt.Errorf("resolving dserver binary path: %w", binaryErr)
	}
	serverCmd := exec.Command(absBinary,
		"-cfg", "none",
		"-pprof", dserverPProfAddr,
		"-port", fmt.Sprintf("%d", dserverSSHPort))
	// Run the server inside the prepared workdir so its ./cache lookups resolve
	// to the authorized_keys and host key set up above.
	serverCmd.Dir = absWorkDir
	fmt.Printf("    Starting dserver (dir %s): %s %s\n", absWorkDir, absBinary,
		strings.Join(serverCmd.Args[1:], " "))
	if cfg.Verbose {
		serverCmd.Stdout = os.Stdout
		serverCmd.Stderr = os.Stderr
	}
	if startErr := serverCmd.Start(); startErr != nil {
		return fmt.Errorf("starting dserver: %w", startErr)
	}
	defer stopServer(serverCmd)

	if readyErr := waitForServerReady(dserverSSHPort); readyErr != nil {
		return readyErr
	}

	// Build the authenticated client invocations once; clients[0] doubles as the
	// upfront auth probe below and the whole set drives the background load.
	clients := serverLoadClients(absTestFiles, dserverSSHPort, dtailWorkloadUser, privateKeyPath)

	// Guard 1: verify auth works before opening the capture window. A single
	// synchronous probe catches a broken keypair/authorized_keys setup up front:
	// if it cannot complete the SSH handshake, every background load client would
	// churn through reconnect crypto and the captured profile would be handshake
	// noise, not streaming work. detectHandshakeFailure is the same signal used
	// by the dtail workload guard.
	if probeErr := probeServerAuth(cfg, clients[0]); probeErr != nil {
		return probeErr
	}

	// Start sustained background load, let it ramp up, then capture while it
	// is flowing. close(stopLoad)+Wait() tears the load generators down.
	stopLoad := make(chan struct{})
	var loadWg sync.WaitGroup
	startServerLoad(cfg, clients, stopLoad, &loadWg)
	time.Sleep(500 * time.Millisecond)

	fmt.Printf("    Capturing CPU profile (%ds) under sustained client load...\n", dserverCaptureSecs)
	captureErr := captureHTTPProfile(dserverPProfAddr, dserverCaptureSecs, profilePath)

	close(stopLoad)
	loadWg.Wait()

	if captureErr != nil {
		return captureErr
	}

	// Guard 2: the HTTP capture always returns a file, so verify the profile is
	// actually dominated by streaming/read work rather than SSH-handshake crypto
	// before it is allowed to ship.
	if verifyErr := verifyDServerProfileRepresentative(profilePath); verifyErr != nil {
		return verifyErr
	}

	fmt.Printf("    Profile captured and saved to %s\n", profilePath)
	return nil
}

// serverLoadClient is a single client invocation used to drive server load.
type serverLoadClient struct {
	cmd  string
	args []string
}

// serverLoadClients returns the authenticated client invocations used to
// generate server-side load. Each connects with --trustAllHosts (the server
// uses an ephemeral host key under "-cfg none"), the plural --servers flag and
// --files (the shape the real DTail clients and integration tests use), plus
// "-user <user> -auth-key-path <privateKey>" so the SSH handshake succeeds
// against the deterministic authorized_keys set up by the workload. Without the
// user/key pair the clients failed auth and the server profile was handshake
// churn (see runDServerWorkload).
func serverLoadClients(testFiles map[string]string, port int, user, keyPath string) []serverLoadClient {
	server := fmt.Sprintf("localhost:%d", port)
	base := []string{
		"-cfg", "none", "-plain", "-trustAllHosts",
		"-user", user, "-auth-key-path", keyPath,
		"-servers", server,
	}
	return []serverLoadClient{
		{"dcat", append(append([]string{}, base...), "-files", testFiles["log"])},
		{"dgrep", append(append([]string{}, base...), "-regex", "ERROR|WARN", "-files", testFiles["log"])},
		{"dmap", append(append([]string{}, base...),
			"-files", testFiles["csv"], "-query", "select status, count(*) group by status")},
	}
}

// probeServerAuth runs a single client synchronously, capturing its combined
// output, and fails if the SSH handshake did not complete. This is the upfront
// representativeness guard for the dserver workload: because the background load
// clients discard their output, a broken auth setup would otherwise only surface
// as a silently handshake-dominated profile. Running one client to completion
// here surfaces the failure loudly before any capture window is opened.
func probeServerAuth(cfg *Config, client serverLoadClient) error {
	binary := filepath.Join(cfg.OutputDir, client.cmd+"-baseline")
	cmd := exec.Command(binary, client.args...)
	var out bytes.Buffer
	if cfg.Verbose {
		cmd.Stdout = io.MultiWriter(os.Stdout, &out)
		cmd.Stderr = io.MultiWriter(os.Stderr, &out)
	} else {
		cmd.Stdout = &out
		cmd.Stderr = &out
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dserver auth probe (%s) failed to run: %w\n%s",
			client.cmd, err, out.String())
	}
	if marker := detectHandshakeFailure(out.String()); marker != "" {
		return fmt.Errorf("dserver load clients did not authenticate (probe %s saw %q): the "+
			"captured profile would be SSH-handshake churn, not streaming/read work; check the "+
			"workload key setup", client.cmd, marker)
	}
	return nil
}

// startServerLoad launches worker goroutines that repeatedly run the given
// authenticated client commands against the server until stop is closed, keeping
// the server busy for the whole profiling window. wg tracks the workers so the
// caller can wait for them to drain before shutting the server down.
func startServerLoad(cfg *Config, clients []serverLoadClient,
	stop <-chan struct{}, wg *sync.WaitGroup) {

	const workers = 4
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				for _, c := range clients {
					select {
					case <-stop:
						return
					default:
					}
					cmd := exec.Command(filepath.Join(cfg.OutputDir, c.cmd+"-baseline"), c.args...)
					cmd.Stdout = io.Discard
					cmd.Stderr = io.Discard
					// Errors are intentionally ignored: this is load
					// generation, not correctness verification (auth is already
					// verified up front by probeServerAuth).
					_ = cmd.Run()
				}
			}
		}()
	}
}

// captureHTTPProfile fetches a CPU profile of the given duration from a running
// server's /debug/pprof endpoint and writes it to outPath.
func captureHTTPProfile(pprofAddr string, seconds int, outPath string) (retErr error) {
	url := fmt.Sprintf("http://%s/debug/pprof/profile?seconds=%d", pprofAddr, seconds)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("capturing profile: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, wrapFileError("close response body for", url, resp.Body.Close()))
	}()

	outFile, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating profile file: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, wrapFileError("close", outPath, outFile.Close()))
	}()

	if _, err := io.Copy(outFile, resp.Body); err != nil {
		return fmt.Errorf("writing profile: %w", err)
	}
	return nil
}

// dserver profile representativeness thresholds. A representative streaming
// profile is dominated by file-read/decompress/channel-write work; a broken-
// auth capture is dominated by the asymmetric SSH *handshake* (rsa.Sign,
// math/big / bigmod, curve25519, tls, sha256), which was ~87% of the garbage
// profile found in the 1v0 review. We deliberately do NOT flag the symmetric
// channel ciphers (aes/chacha20/poly1305): encrypting streamed data is exactly
// the representative work we expect, so those count as stream frames. The
// heuristic is intentionally conservative -- it must catch the ~87%-crypto case
// without false-positiving on a healthy profile, which always carries syscall/
// poll/read frames near the top.
const (
	dserverProfileTopNodes     = 30
	dserverHandshakeFailPct    = 40.0 // handshake share that fails when NO stream frame is present
	dserverHandshakeExtremePct = 60.0 // handshake share that fails regardless of stream frames
)

// verifyDServerProfileRepresentative fails when the captured dserver profile is
// dominated by SSH-handshake crypto rather than streaming/read work. It shells
// out to "go tool pprof -top" (github.com/google/pprof is intentionally not a
// dependency) and applies the classifyProfileTop heuristic. This is the
// post-capture backstop for the dserver workload: the HTTP /debug/pprof capture
// always returns a file, so a handshake-churn profile would otherwise ship
// silently even though it is useless for optimizing the streaming path.
func verifyDServerProfileRepresentative(profilePath string) error {
	out, err := exec.Command("go", "tool", "pprof",
		fmt.Sprintf("-nodecount=%d", dserverProfileTopNodes), "-top", profilePath).Output()
	if err != nil {
		return fmt.Errorf("running go tool pprof -top on %s: %w", profilePath, err)
	}
	handshakePct, hasStream := classifyProfileTop(string(out))
	if (handshakePct >= dserverHandshakeFailPct && !hasStream) ||
		handshakePct >= dserverHandshakeExtremePct {
		return fmt.Errorf("dserver profile %s is handshake-dominated (%.1f%% asymmetric "+
			"crypto/handshake in top %d frames, stream frames present=%v): the load clients did "+
			"not authenticate, so the capture is SSH-handshake churn, not streaming/read work; "+
			"check the workload key setup", profilePath, handshakePct, dserverProfileTopNodes,
			hasStream)
	}
	fmt.Printf("    Profile representativeness OK (%.1f%% handshake crypto in top %d, stream frames present=%v)\n",
		handshakePct, dserverProfileTopNodes, hasStream)
	return nil
}

// classifyProfileTop parses "go tool pprof -top" output and returns the sum of
// flat% attributed to asymmetric-SSH-handshake frames and whether any
// streaming/read frame appears in the listing. The -top format is one function
// per line, with the flat percentage in the second column:
//
//	 flat  flat%   sum%        cum   cum%
//	0.50s 50.00% 50.00%      0.50s 50.00%  crypto/rsa.(*PrivateKey).Sign
//
// We sum the flat% of lines naming handshake work (asymmetric crypto + TLS/SSH
// handshake + the big-integer/curve/hash primitives that drive it) and flag the
// presence of any read/stream frame (syscalls, file/poll IO, buffering,
// (de)compression, the symmetric channel ciphers, and the dtail server/mapr
// packages). Substring matching keeps this dependency-free.
func classifyProfileTop(top string) (handshakePct float64, hasStream bool) {
	handshakeMarkers := []string{
		"crypto/rsa", "math/big", "bigmod", "crypto/tls", "curve25519",
		"crypto/ecdsa", "crypto/ed25519", "crypto/sha256", "crypto/sha512",
		"handshake",
	}
	streamMarkers := []string{
		"syscall", "internal/poll", "os.(*file)", "bufio", "compress",
		"gzip", "zstd", "crypto/cipher", "crypto/aes", "chacha20", "poly1305",
		"mapr", "logformat", "/internal/server", "handlers", "io.copy",
		"bytes.", "scanner",
	}
	for _, line := range strings.Split(top, "\n") {
		fields := strings.Fields(line)
		// A data row has at least: flat flat% sum% cum cum% name.
		if len(fields) < 6 {
			continue
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
		if err != nil {
			// Header and separator lines have no numeric second column.
			continue
		}
		lower := strings.ToLower(line)
		for _, m := range handshakeMarkers {
			if strings.Contains(lower, m) {
				handshakePct += pct
				break
			}
		}
		for _, m := range streamMarkers {
			if strings.Contains(lower, m) {
				hasStream = true
				break
			}
		}
	}
	return handshakePct, hasStream
}

// waitForServerReady blocks until the dserver SSH port accepts TCP connections
// or a timeout elapses, so client load is not fired before the server is up.
func waitForServerReady(port int) error {
	addr := fmt.Sprintf("localhost:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("dserver SSH port %d not ready within timeout", port)
}

// stopServer signals the server to shut down gracefully, then kills it if it
// does not exit promptly.
func stopServer(serverCmd *exec.Cmd) {
	if serverCmd.Process == nil {
		return
	}
	_ = serverCmd.Process.Signal(os.Interrupt)
	time.Sleep(300 * time.Millisecond)
	_ = serverCmd.Process.Kill()
	_ = serverCmd.Wait()
}

func mergeProfiles(profiles []string, output string) error {
	if len(profiles) == 0 {
		return fmt.Errorf("no profiles to merge")
	}

	// Filter out empty profiles
	var validProfiles []string
	for _, profile := range profiles {
		info, err := os.Stat(profile)
		if err != nil {
			continue
		}
		if info.Size() > 0 {
			validProfiles = append(validProfiles, profile)
		}
	}

	if len(validProfiles) == 0 {
		// Previously this silently wrote an empty output file so the workflow
		// could continue - which is exactly how the 0-byte dtail.pprof was
		// produced and then committed. Fail loudly instead so a workload that
		// fails to exercise the binary can never masquerade as a valid profile.
		return fmt.Errorf("all %d captured profiles are empty; the workload did not "+
			"produce CPU samples (check that the binary was actually driven under load)",
			len(profiles))
	}

	if len(validProfiles) == 1 {
		fmt.Printf("    Using single profile (no merge needed)\n")
		// Just rename
		return os.Rename(validProfiles[0], output)
	}

	fmt.Printf("    Merging %d profiles...\n", len(validProfiles))
	// Use go tool pprof to merge
	args := append([]string{"tool", "pprof", "-proto"}, validProfiles...)
	cmd := exec.Command("go", args...)
	fmt.Printf("    Command: go %s\n", strings.Join(args, " "))

	outFile, err := os.Create(output)
	if err != nil {
		return err
	}
	cmd.Stdout = outFile
	runErr := cmd.Run()
	closeErr := outFile.Close()
	return errors.Join(
		wrapFileError("write merged profile to", output, runErr),
		wrapFileError("close", output, closeErr),
	)
}

func buildWithPGO(cfg *Config) error {
	for _, cmd := range cfg.Commands {
		profilePath := filepath.Join(cfg.ProfileDir, fmt.Sprintf("%s.pprof", cmd))

		// Check if profile exists and is not empty
		info, err := os.Stat(profilePath)
		if err != nil {
			fmt.Printf("Warning: No profile found for %s, skipping PGO build\n", cmd)
			continue
		}
		if info.Size() == 0 {
			fmt.Printf("Warning: Profile for %s is empty, skipping PGO build\n", cmd)
			continue
		}

		if cfg.Verbose {
			fmt.Printf("Building %s with PGO...\n", cmd)
		}

		// Build with PGO
		buildCmd := exec.Command("go", "build",
			"-pgo", profilePath,
			"-o", filepath.Join(cfg.OutputDir, cmd),
			fmt.Sprintf("./cmd/%s", cmd))

		if cfg.Verbose {
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
		}

		if err := buildCmd.Run(); err != nil {
			return fmt.Errorf("building %s with PGO: %w", cmd, err)
		}
	}

	return nil
}

func comparePerformance(cfg *Config) error {
	// Generate small test data for quick benchmark
	testFiles, err := generateSmallTestData()
	if err != nil {
		return err
	}
	defer cleanupTestData(testFiles)

	fmt.Println("\nPerformance Comparison:")
	fmt.Println("----------------------")

	for _, cmd := range cfg.Commands {
		baseline := filepath.Join(cfg.OutputDir, cmd+"-baseline")
		optimized := filepath.Join(cfg.OutputDir, cmd)

		// Skip if either binary doesn't exist
		if _, err := os.Stat(baseline); err != nil {
			continue
		}
		if _, err := os.Stat(optimized); err != nil {
			continue
		}

		fmt.Printf("\n%s:\n", cmd)

		// Run benchmark
		fmt.Printf("  Running baseline benchmark...\n")
		baselineTime, err := benchmarkCommand(baseline, cmd, testFiles)
		if err != nil {
			return fmt.Errorf("run baseline benchmark for %s: %w", cmd, err)
		}
		fmt.Printf("  Running optimized benchmark...\n")
		optimizedTime, err := benchmarkCommand(optimized, cmd, testFiles)
		if err != nil {
			return fmt.Errorf("run optimized benchmark for %s: %w", cmd, err)
		}

		if baselineTime > 0 && optimizedTime > 0 {
			improvement := (float64(baselineTime) - float64(optimizedTime)) / float64(baselineTime) * 100
			fmt.Printf("  Baseline:  %.3fs\n", baselineTime.Seconds())
			fmt.Printf("  Optimized: %.3fs\n", optimizedTime.Seconds())
			fmt.Printf("  Improvement: %.1f%%\n", improvement)
		}
	}

	return nil
}

func benchmarkCommand(binary, command string, testFiles map[string]string) (time.Duration, error) {
	var cmd *exec.Cmd

	switch command {
	case "dcat":
		cmd = exec.Command(binary, "-cfg", "none", "-plain", testFiles["log"])
	case "dgrep":
		cmd = exec.Command(binary, "-cfg", "none", "-plain", "-regex", "ERROR", testFiles["log"])
	case "dmap":
		cmd = exec.Command(binary, "-cfg", "none", "-plain", "-files", testFiles["csv"],
			"-query", "select count(*)")
	default:
		return 0, fmt.Errorf("unknown benchmark command %q", command)
	}

	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func generateTestData(cfg *Config) (map[string]string, error) {
	files := make(map[string]string)

	// Generate log file
	logFile := filepath.Join(cfg.ProfileDir, "test.log")
	if err := common.GenerateLogFile(logFile, cfg.TestDataSize); err != nil {
		return nil, err
	}
	files["log"] = logFile

	// Generate CSV file
	csvFile := filepath.Join(cfg.ProfileDir, "test.csv")
	if err := common.GenerateCSVFile(csvFile, cfg.TestDataSize/10); err != nil {
		return nil, err
	}
	files["csv"] = csvFile

	return files, nil
}

func generateSmallTestData() (map[string]string, error) {
	files := make(map[string]string)

	// Generate small files for quick benchmarks
	logFile := "/tmp/pgo_bench.log"
	if err := common.GenerateLogFile(logFile, 10000); err != nil {
		return nil, err
	}
	files["log"] = logFile

	csvFile := "/tmp/pgo_bench.csv"
	if err := common.GenerateCSVFile(csvFile, 1000); err != nil {
		return nil, err
	}
	files["csv"] = csvFile

	return files, nil
}

func cleanupTestData(files map[string]string) {
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("Unable to remove PGO test data %s: %v", f, err)
		}
	}
}
