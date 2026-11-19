package integrationtests

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestDMap1(t *testing.T) {
	skipIfNotIntegrationTest(t)
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap1")
	defer testLogger.WriteLogFile()

	testTable := map[string]string{
		"a": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname",
		"b": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname where lifetimeConnections >= 3",
		"c": "from STATS select count($line),last($time)," +
			"avg($goroutines),min(concurrentConnections),max(lifetimeConnections) " +
			"group by $hostname where $time eq \"1002-071949\"",
		"d": "from STATS select $mask,$md5,$foo,$bar,$baz,last($time)," +
			" set $mask = maskdigits($time), $md5 = md5sum($time), " +
			"$foo = 42, $bar = \"baz\", $baz = $time group by $hostname",
	}

	// Test in serverless mode
	t.Run("Serverless", func(t *testing.T) {
		for subtestName, query := range testTable {
			t.Run(subtestName, func(t *testing.T) {
				t.Log("Testing dmap with input file")
				testDmap1Serverless(t, testLogger, query, subtestName, false)

				t.Log("Testing dmap with stdin input pipe")
				testDmap1Serverless(t, testLogger, query, subtestName, true)
			})
		}
	})

	// Test in server mode
	t.Run("ServerMode", func(t *testing.T) {
		for subtestName, query := range testTable {
			t.Run(subtestName, func(t *testing.T) {
				t.Log("Testing dmap with input file in server mode")
				testDmap1WithServer(t, testLogger, query, subtestName)
			})
		}
	})
}

func testDmap1Serverless(t *testing.T, logger *TestLogger, query, subtestName string, usePipe bool) {
	paths := GetStandardTestPaths()
	csvFile := fmt.Sprintf("dmap1%s.csv.tmp", subtestName)
	expectedCsvFile := fmt.Sprintf("dmap1%s.csv.expected", subtestName)
	queryFile := fmt.Sprintf("%s.query", csvFile)
	query = fmt.Sprintf("%s outfile %s", query, csvFile)

	cleanupFiles(t, csvFile, queryFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()

	var stdoutCh, stderrCh <-chan string
	var cmdErrCh <-chan error
	var err error

	args := NewCommandArgs()
	args.Logger = "stdout"
	args.LogLevel = "info"
	args.NoColor = true
	args.ExtraArgs = []string{"--query", query}

	if usePipe {
		stdoutCh, stderrCh, cmdErrCh, err = startCommand(ctx, t,
			paths.MaprTestData, "../dmap", args.ToSlice()...)
	} else {
		stdoutCh, stderrCh, cmdErrCh, err = startCommand(ctx, t,
			"", "../dmap", append(args.ToSlice(), paths.MaprTestData)...)
	}

	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDmap1WithServer(t *testing.T, logger *TestLogger, query, subtestName string) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	csvFile := fmt.Sprintf("dmap1%s.csv.tmp", subtestName)
	expectedCsvFile := fmt.Sprintf("dmap1%s.csv.expected", subtestName)
	queryFile := fmt.Sprintf("%s.query", csvFile)
	query = fmt.Sprintf("%s outfile %s", query, csvFile)

	cleanupFiles(t, csvFile, queryFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	args := NewCommandArgs()
	args.Logger = "stdout"
	args.LogLevel = "info"
	args.NoColor = true
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.Files = []string{paths.MaprTestData}
	args.ExtraArgs = []string{"--query", query}

	stdoutCh, stderrCh, cmdErrCh, err := startCommand(server.ctx, t,
		"", "../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	waitForCommand(server.ctx, t, stdoutCh, stderrCh, cmdErrCh)

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func TestDMap2(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap2")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMap2",
		ServerlessTest: func(t *testing.T) { testDMap2Serverless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMap2WithServer(t, testLogger) },
	})
}

func TestDMapOutfileArbitraryPath(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMapOutfileArbitraryPath")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name: "TestDMapOutfileArbitraryPath",
		ServerlessTest: func(t *testing.T) {
			testDMapOutfileArbitraryPathServerless(t, testLogger)
		},
		ServerTest: func(t *testing.T) {
			testDMapOutfileArbitraryPathWithServer(t, testLogger)
		},
	})
}

func testDMapOutfileArbitraryPathServerless(t *testing.T, logger *TestLogger) {
	paths := GetStandardTestPaths()
	outFile := "dmap_outfile_arbitrary_serverless.stdout.tmp"
	csvFile := filepath.Join(t.TempDir(), "dmap_outfile_arbitrary_serverless.csv")
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	if !filepath.IsAbs(csvFile) {
		t.Fatalf("expected absolute outfile path, got %q", csvFile)
	}
	cleanupFiles(t, outFile, csvFile, queryFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()

	_, err := runCommand(ctx, t, outFile,
		"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMapOutfileArbitraryPathWithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	outFile := "dmap_outfile_arbitrary_server.stdout.tmp"
	csvFile := filepath.Join(t.TempDir(), "dmap_outfile_arbitrary_server.csv")
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	if !filepath.IsAbs(csvFile) {
		t.Fatalf("expected absolute outfile path, got %q", csvFile)
	}
	cleanupFiles(t, outFile, csvFile, queryFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	args := NewCommandArgs()
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = []string{paths.MaprTestData}
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(server.ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMap2Serverless(t *testing.T, logger *TestLogger) {
	paths := GetStandardTestPaths()
	outFile := "dmap2_serverless.stdout.tmp"
	csvFile := "dmap2_serverless.csv.tmp"
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	cleanupFiles(t, outFile, csvFile, queryFile)

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()
	_, err := runCommand(ctx, t, outFile,
		"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMap2WithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	outFile := "dmap2_server.stdout.tmp"
	csvFile := "dmap2_server.csv.tmp"
	expectedCsvFile := "dmap2.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	cleanupFiles(t, outFile, csvFile, queryFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
		"avg($goroutines),min($goroutines) group by $time order by count($time) "+
		"outfile %s", csvFile)

	args := NewCommandArgs()
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = []string{paths.MaprTestData}
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(server.ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func TestDMap3(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap3")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMap3",
		ServerlessTest: func(t *testing.T) { testDMap3Serverless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMap3WithServer(t, testLogger) },
	})
}

func testDMap3Serverless(t *testing.T, logger *TestLogger) {
	paths := GetStandardTestPaths()
	outFile := "dmap3_serverless.stdout.tmp"
	csvFile := "dmap3_serverless.csv.tmp"
	expectedCsvFile := "dmap3.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	cleanupFiles(t, outFile, csvFile, queryFile)

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),avg($goroutines),min($goroutines) "+
		"group by $time order by count($time) desc "+
		"outfile %s", csvFile)

	// Create a large list of input files
	var inputFiles []string
	for i := 0; i < 100; i++ {
		inputFiles = append(inputFiles, paths.MaprTestData)
	}

	// Simply run dmap with multiple input files directly
	// Use longer timeout for processing 100 files
	ctxTimeout, cancel := createTestContextWithLongTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()

	args := NewCommandArgs()
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(ctx, t, outFile,
		"../dmap", append(args.ToSlice(), inputFiles...)...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMap3WithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	outFile := "dmap3_server.stdout.tmp"
	csvFile := "dmap3_server.csv.tmp"
	expectedCsvFile := "dmap3.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	cleanupFiles(t, outFile, csvFile, queryFile)

	server := NewTestServer(t)
	// Correctness test for server-mode MapReduce over 100 files: the CSV output
	// must equal the expected file. This is not a turbo-vs-non-turbo comparison.
	// Under DTAIL_INTEGRATION_TEST_RUN_MODE the server force-disables turbo boost
	// (see internal/config/initializer.go), so this dserver always runs the
	// non-turbo aggregate path regardless of any DTAIL_TURBOBOOST_* env var.
	// Turbo aggregate correctness is covered by the unit tests in
	// internal/mapr/server/turbo_aggregate_test.go.
	cfg := &ServerConfig{
		Port:        server.port,
		BindAddress: server.bindAddress,
		LogLevel:    "error",
		ExtraArgs:   []string{"--cfg", "test_server_100files.json"},
	}
	if err := server.StartWithConfig(cfg); err != nil {
		t.Error(err)
		return
	}

	query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),avg($goroutines),min($goroutines) "+
		"group by $time order by count($time) desc "+
		"outfile %s", csvFile)

	// Create a large list of input files
	var inputFiles []string
	for i := 0; i < 100; i++ {
		inputFiles = append(inputFiles, paths.MaprTestData)
	}

	args := NewCommandArgs()
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = inputFiles
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(server.ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func TestDMap4Append(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap4Append")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMap4Append",
		ServerlessTest: func(t *testing.T) { testDMap4AppendServerless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMap4AppendWithServer(t, testLogger) },
	})
}

func testDMap4AppendServerless(t *testing.T, logger *TestLogger) {
	paths := GetStandardTestPaths()
	csvFile := "dmap4_serverless.csv.tmp"
	queryFile := fmt.Sprintf("%s.query", csvFile)

	// Clean up files once at the beginning
	cleanupFiles(t, csvFile, queryFile)

	t.Run("FirstQuery", func(t *testing.T) {
		stdout := "dmap4_serverless.stdout1.tmp"
		cleanupFiles(t, stdout)

		// First query
		query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by count($time) "+
			"outfile %s", csvFile)

		ctx, cancel := createTestContextWithTimeout(t)
		defer cancel()
		_, err := runCommand(ctx, t, stdout,
			"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}

		// Verify the query file
		if err := verifyQueryFile(t, queryFile, query); err != nil {
			t.Error(err)
		}
	})

	t.Run("SecondQueryWithAppend", func(t *testing.T) {
		stdout := "dmap4_serverless.stdout2.tmp"
		cleanupFiles(t, stdout)

		// Second query with append
		query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by avg($goroutines) reverse "+
			"outfile append:%s", csvFile)

		ctx, cancel := createTestContextWithTimeout(t)
		defer cancel()
		_, err := runCommand(ctx, t, stdout,
			"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output (should still be the first query result - append doesn't change existing file)
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}
	})

	t.Run("ThirdQueryWithAppend", func(t *testing.T) {
		stdout := "dmap4_serverless.stdout3.tmp"
		cleanupFiles(t, stdout)

		// Third query with append (different structure)
		query := fmt.Sprintf("from STATS select count($line),$hostname "+
			"group by $hostname "+
			"outfile append:%s", csvFile)

		ctx, cancel := createTestContextWithTimeout(t)
		defer cancel()
		_, err := runCommand(ctx, t, stdout,
			"../dmap", "--query", query, "--cfg", "none", paths.MaprTestData)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output (should still be the first query result - append doesn't change existing file)
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}

		// For append test, the query file should still contain the first query
		firstQuery := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by count($time) "+
			"outfile %s", csvFile)
		if err := verifyQueryFile(t, queryFile, firstQuery); err != nil {
			t.Error(err)
		}
	})
}

func testDMap4AppendWithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	paths := GetStandardTestPaths()
	csvFile := "dmap4_server.csv.tmp"
	queryFile := fmt.Sprintf("%s.query", csvFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	baseArgs := NewCommandArgs()
	baseArgs.Servers = []string{server.Address()}
	baseArgs.TrustAllHosts = true
	baseArgs.NoColor = true
	baseArgs.Files = []string{paths.MaprTestData}

	// Clean up files once at the beginning
	cleanupFiles(t, csvFile, queryFile)

	t.Run("FirstQuery", func(t *testing.T) {
		stdout := "dmap4_server.stdout1.tmp"
		cleanupFiles(t, stdout)

		// First query
		query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by count($time) "+
			"outfile %s", csvFile)

		args := *baseArgs
		args.ExtraArgs = []string{"--query", query}

		_, err := runCommand(server.ctx, t, stdout,
			"../dmap", args.ToSlice()...)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}

		// Verify the query file
		if err := verifyQueryFile(t, queryFile, query); err != nil {
			t.Error(err)
		}
	})

	t.Run("SecondQueryWithAppend", func(t *testing.T) {
		stdout := "dmap4_server.stdout2.tmp"
		cleanupFiles(t, stdout)

		// Second query with append
		query := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by avg($goroutines) reverse "+
			"outfile append:%s", csvFile)

		args := *baseArgs
		args.ExtraArgs = []string{"--query", query}

		_, err := runCommand(server.ctx, t, stdout,
			"../dmap", args.ToSlice()...)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output (should still be the first query result - append doesn't change existing file)
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}
	})

	t.Run("ThirdQueryWithAppend", func(t *testing.T) {
		stdout := "dmap4_server.stdout3.tmp"
		cleanupFiles(t, stdout)

		// Third query with append (different structure)
		query := fmt.Sprintf("from STATS select count($line),$hostname "+
			"group by $hostname "+
			"outfile append:%s", csvFile)

		args := *baseArgs
		args.ExtraArgs = []string{"--query", query}

		_, err := runCommand(server.ctx, t, stdout,
			"../dmap", args.ToSlice()...)
		if err != nil {
			t.Error(err)
			return
		}

		// Verify the CSV output (should still be the first query result - append doesn't change existing file)
		if err := compareFilesContentsWithContext(ctx, t, csvFile, "dmap4_query1.csv.expected"); err != nil {
			t.Error(err)
		}

		// For append test, the query file should still contain the first query
		firstQuery := fmt.Sprintf("from STATS select count($time),$time,max($goroutines),"+
			"avg($goroutines),min($goroutines) group by $time order by count($time) "+
			"outfile %s", csvFile)
		if err := verifyQueryFile(t, queryFile, firstQuery); err != nil {
			t.Error(err)
		}
	})
}

func TestDMap5CSV(t *testing.T) {
	cleanupTmpFiles(t)
	testLogger := NewTestLogger("TestDMap5CSV")
	defer testLogger.WriteLogFile()
	runDualModeTest(t, DualModeTest{
		Name:           "TestDMap5CSV",
		ServerlessTest: func(t *testing.T) { testDMap5CSVServerless(t, testLogger) },
		ServerTest:     func(t *testing.T) { testDMap5CSVWithServer(t, testLogger) },
	})
}

func testDMap5CSVServerless(t *testing.T, logger *TestLogger) {
	inFile := "dmap5.csv.in"
	csvFile := "dmap5_serverless.csv.tmp"
	expectedCsvFile := "dmap5.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap5_serverless.stdout.tmp"
	cleanupFiles(t, csvFile, queryFile, outFile)

	query := fmt.Sprintf("select sum($timecount),last($time),min($min_goroutines) "+
		"group by $hostname set $timecount = `count($time)`, $time = `$time`, "+
		"$min_goroutines = `min($goroutines)` logformat csv outfile %s", csvFile)

	ctxTimeout, cancel := createTestContextWithTimeout(t)
	ctx := WithTestLogger(ctxTimeout, logger)
	defer cancel()
	_, err := runCommand(ctx, t, outFile,
		"../dmap", "--query", query, "--cfg", "none", inFile)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	// Verify the query file contains the expected query
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}

func testDMap5CSVWithServer(t *testing.T, logger *TestLogger) {
	ctx := WithTestLogger(context.Background(), logger)
	inFile := "dmap5.csv.in"
	csvFile := "dmap5_server.csv.tmp"
	expectedCsvFile := "dmap5.csv.expected"
	queryFile := fmt.Sprintf("%s.query", csvFile)
	outFile := "dmap5_server.stdout.tmp"
	cleanupFiles(t, csvFile, queryFile, outFile)

	server := NewTestServer(t)
	if err := server.Start("error"); err != nil {
		t.Error(err)
		return
	}

	query := fmt.Sprintf("select sum($timecount),last($time),min($min_goroutines) "+
		"group by $hostname set $timecount = `count($time)`, $time = `$time`, "+
		"$min_goroutines = `min($goroutines)` logformat csv outfile %s", csvFile)

	args := NewCommandArgs()
	args.Servers = []string{server.Address()}
	args.TrustAllHosts = true
	args.NoColor = true
	args.Files = []string{inFile}
	args.ExtraArgs = []string{"--query", query}

	_, err := runCommand(server.ctx, t, outFile,
		"../dmap", args.ToSlice()...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContentsWithContext(ctx, t, csvFile, expectedCsvFile); err != nil {
		t.Error(err)
	}
	// Verify the query file contains the expected query
	if err := verifyQueryFile(t, queryFile, query); err != nil {
		t.Error(err)
	}
}
