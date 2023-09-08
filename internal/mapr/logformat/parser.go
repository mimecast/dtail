package logformat

import (
	"errors"
	"fmt"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/mapr"
)

// ErrIgnoreFields indicates that the fields should be ignored.
var ErrIgnoreFields error = errors.New("Ignore this field set")

// Parser is used to parse the mapreduce information from the server log files.
type Parser interface {
	// MakeFields creates a field map from an input log line.
	MakeFields(string) (map[string]string, error)
}

// NewParser returns a new log parser.
func NewParser(logFormatName string, query *mapr.Query) (Parser, error) {
	hostname, err := config.Hostname()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	timeZoneName, timeZoneOffset := now.Zone()

	// Extend this for adding more log formats!
	switch logFormatName {
	case "generic":
		return newGenericParser(hostname, timeZoneName, timeZoneOffset)
	case "generickv":
		return newGenericKVParser(hostname, timeZoneName, timeZoneOffset)
	case "csv":
		return newCSVParser(hostname, timeZoneName, timeZoneOffset)
	case "mimecast":
		return newMimecastParser(hostname, timeZoneName, timeZoneOffset)
	case "mimecastgeneric":
		return newMimecastGenericParser(hostname, timeZoneName, timeZoneOffset)
	case "default":
		return newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	case "custom1":
		return newCustom1Parser(hostname, timeZoneName, timeZoneOffset)
	case "custom2":
		return newCustom2Parser(hostname, timeZoneName, timeZoneOffset)
	default:
		p, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
		if err != nil {
			return p, fmt.Errorf("No '%s' mapr log format and problem creating default one: %v",
				logFormatName, err)
		}
		return p, fmt.Errorf("No '%s' mapr log format", logFormatName)
	}
}
