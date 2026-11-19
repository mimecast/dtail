package logformat

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/mapr"
)

// ErrIgnoreFields indicates that the fields should be ignored.
var ErrIgnoreFields error = errors.New("ignore this field set")

// Parser is used to parse the mapreduce information from the server log files.
type Parser interface {
	// MakeFields creates a field map from an input log line. The sourceID
	// identifies the log file (or stream) the line belongs to so that
	// stateful parsers (e.g. CSV with per-file headers) can key their
	// state per source instead of smearing it across every file in a
	// session.
	MakeFields(maprLine, sourceID string) (map[string]string, error)
}

type queryAwareParser interface {
	setQuery(*mapr.Query)
}

// ParserFactory builds a Parser for a specific log format.
type ParserFactory func(hostname, timeZoneName string, timeZoneOffset int) (Parser, error)

var parserFactories = make(map[string]ParserFactory)
var parserFactoriesMu sync.RWMutex

func init() {
	registerBuiltInParsers()
}

// RegisterParser registers or replaces a parser factory for a log format name.
func RegisterParser(logFormatName string, factory ParserFactory) error {
	name := strings.TrimSpace(logFormatName)
	if name == "" {
		return errors.New("log format name cannot be empty")
	}
	if factory == nil {
		return errors.New("parser factory cannot be nil")
	}

	parserFactoriesMu.Lock()
	defer parserFactoriesMu.Unlock()
	parserFactories[name] = factory
	return nil
}

func getParserFactory(logFormatName string) (ParserFactory, bool) {
	parserFactoriesMu.RLock()
	defer parserFactoriesMu.RUnlock()
	factory, found := parserFactories[logFormatName]
	return factory, found
}

func registerBuiltInParsers() {
	mustRegisterParser("generic", wrapParserFactory(newGenericParser))
	mustRegisterParser("generickv", wrapParserFactory(newGenericKVParser))
	mustRegisterParser("csv", wrapParserFactory(newCSVParser))
	mustRegisterParser("mimecast", wrapParserFactory(newMimecastParser))
	mustRegisterParser("mimecastgeneric", wrapParserFactory(newMimecastGenericParser))
	mustRegisterParser("default", wrapParserFactory(newDefaultParser))
	mustRegisterParser("custom1", wrapParserFactory(newCustom1Parser))
	mustRegisterParser("custom2", wrapParserFactory(newCustom2Parser))
}

func mustRegisterParser(logFormatName string, factory ParserFactory) {
	if err := RegisterParser(logFormatName, factory); err != nil {
		panic(err)
	}
}

func wrapParserFactory[T Parser](factory func(string, string, int) (T, error)) ParserFactory {
	return func(hostname, timeZoneName string, timeZoneOffset int) (Parser, error) {
		return factory(hostname, timeZoneName, timeZoneOffset)
	}
}

// NewParser returns a new log parser.
func NewParser(logFormatName string, query *mapr.Query) (Parser, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return NewParserWithHostname(logFormatName, query, hostname)
}

// NewParserWithHostname returns a log parser using the supplied process hostname.
func NewParserWithHostname(logFormatName string, query *mapr.Query, hostname string) (Parser, error) {
	now := time.Now()
	timeZoneName, timeZoneOffset := now.Zone()

	parserFactory, found := getParserFactory(logFormatName)
	if !found {
		return nil, fmt.Errorf("no '%s' mapr log format", logFormatName)
	}

	selectedParser, parserErr := parserFactory(hostname, timeZoneName, timeZoneOffset)
	if parserErr != nil {
		return nil, fmt.Errorf("create %q mapr log format parser: %w", logFormatName, parserErr)
	}
	if isNilParser(selectedParser) {
		return nil, fmt.Errorf("create %q mapr log format parser: factory returned nil parser",
			logFormatName)
	}
	configureParserQuery(selectedParser, query)
	return selectedParser, nil
}

func configureParserQuery(parser Parser, query *mapr.Query) {
	if isNilParser(parser) {
		return
	}
	queryAware, ok := parser.(queryAwareParser)
	if !ok {
		return
	}
	queryAware.setQuery(query)
}

func isNilParser(parser Parser) bool {
	if parser == nil {
		return true
	}

	value := reflect.ValueOf(parser)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.UnsafePointer:
		return value.IsZero()
	default:
		return false
	}
}
