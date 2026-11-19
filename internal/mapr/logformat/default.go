package logformat

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

var _ FieldsIntoParser = (*defaultParser)(nil)
var _ Prober = (*defaultParser)(nil)
var _ ProbeTabler = (*defaultParser)(nil)

// defaultTableToken is the index of the MAPREDUCE:<TABLE> token in a line of
// the default format (see MakeFieldsInto).
const defaultTableToken = 9

type defaultParser struct {
	hostname       string
	timeZoneName   string
	timeZoneOffset string
	fieldsCapacity int

	wantStar       bool
	wantLine       bool
	wantEmpty      bool
	wantHostname   bool
	wantServer     bool
	wantTimezone   bool
	wantTimeOffset bool
	wantSeverity   bool
	wantLogLevel   bool
	wantTime       bool
	wantDate       bool
	wantHour       bool
	wantMinute     bool
	wantSecond     bool
	wantPID        bool
	wantCaller     bool
	wantCPUs       bool
	wantGoroutines bool
	wantCGOCalls   bool
	wantLoadAvg    bool
	wantUptime     bool

	allDynamicFields bool
	dynamicFields    map[string]struct{}
}

func newDefaultParser(hostname, timeZoneName string, timeZoneOffset int) (*defaultParser, error) {
	parser := &defaultParser{
		hostname:       hostname,
		timeZoneName:   timeZoneName,
		timeZoneOffset: fmt.Sprintf("%d", timeZoneOffset),
	}
	parser.configureFieldPlan(mapr.ParserFieldPlan{AllFields: true})
	return parser, nil
}

func (p *defaultParser) setQuery(query *mapr.Query) {
	p.configureFieldPlan(query.ParserFieldPlan())
}

// MakeFields allocates a field map and parses maprLine into it.
func (p *defaultParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	fields := make(map[string]string, p.fieldsCapacity)
	if err := p.MakeFieldsInto(fields, maprLine, sourceID); err != nil {
		if errors.Is(err, ErrIgnoreFields) {
			return nil, err
		}
		return fields, err
	}
	return fields, nil
}

// MakeFieldsInto parses maprLine into the caller's map, reusing its storage.
func (p *defaultParser) MakeFieldsInto(dst map[string]string, maprLine, _ string) error {
	clear(dst)
	fields := dst
	tokenIndex := 0
	start := 0

	for {
		token, next, done := protocol.ScanField(maprLine, start)
		switch tokenIndex {
		case 0:
			if !strings.HasPrefix(token, "INFO") {
				return ErrIgnoreFields
			}
			p.addDefaultFields(fields, maprLine)
			if p.wantSeverity {
				fields["$severity"] = token
			}
			if p.wantLogLevel {
				fields["$loglevel"] = token
			}
		case 1:
			if p.wantTime {
				fields["$time"] = token
			}
			if len(token) == 15 {
				// Example: 20211002-071209
				if p.wantDate {
					fields["$date"] = token[0:8]
				}
				if p.wantHour {
					fields["$hour"] = token[9:11]
				}
				if p.wantMinute {
					fields["$minute"] = token[11:13]
				}
				if p.wantSecond {
					fields["$second"] = token[13:]
				}
			} else if len(token) == 11 {
				// dserver stamps its own diagnostics MMDD-HHMMSS (11 chars, see
				// internal/io/dlog/dlog.go), which carries no year and so no
				// $date. Example: 1002-071143
				if p.wantHour {
					fields["$hour"] = token[5:7]
				}
				if p.wantMinute {
					fields["$minute"] = token[7:9]
				}
				if p.wantSecond {
					fields["$second"] = token[9:11]
				}
			}
		case 2:
			if p.wantPID {
				fields["$pid"] = token
			}
		case 3:
			if p.wantCaller {
				fields["$caller"] = token
			}
		case 4:
			if p.wantCPUs {
				fields["$cpus"] = token
			}
		case 5:
			if p.wantGoroutines {
				fields["$goroutines"] = token
			}
		case 6:
			if p.wantCGOCalls {
				fields["$cgocalls"] = token
			}
		case 7:
			if p.wantLoadAvg {
				fields["$loadavg"] = token
			}
		case 8:
			if p.wantUptime {
				fields["$uptime"] = token
			}
		case defaultTableToken:
			if !strings.HasPrefix(token, "MAPREDUCE:") {
				return ErrIgnoreFields
			}
		default:
			if err := p.addKeyValueField(fields, token); err != nil {
				return err
			}
		}

		tokenIndex++
		if done {
			break
		}
		start = next
	}

	if tokenIndex < 11 {
		// Not a DTail mapreduce log line.
		return ErrIgnoreFields
	}

	return nil
}

func (p *defaultParser) addDefaultFields(fields map[string]string, maprLine string) {
	if p.wantStar {
		fields["*"] = "*"
	}
	if p.wantLine {
		fields["$line"] = maprLine
	}
	if p.wantEmpty {
		fields["$empty"] = ""
	}
	if p.wantHostname {
		fields["$hostname"] = p.hostname
	}
	if p.wantServer {
		fields["$server"] = p.hostname
	}
	if p.wantTimezone {
		fields["$timezone"] = p.timeZoneName
	}
	if p.wantTimeOffset {
		fields["$timeoffset"] = p.timeZoneOffset
	}
}

func (p *defaultParser) addDynamicField(fields map[string]string, key string, value string) {
	if p.allDynamicFields {
		fields[key] = value
		return
	}
	if _, ok := p.dynamicFields[key]; ok {
		fields[key] = value
	}
}

func (p *defaultParser) addKeyValueField(fields map[string]string, token string) error {
	keyAndValueIndex := strings.IndexByte(token, '=')
	if keyAndValueIndex < 0 {
		return fmt.Errorf("unable to parse key-value token '%s'", token)
	}
	p.addDynamicField(fields, token[:keyAndValueIndex], token[keyAndValueIndex+1:])
	return nil
}

func (p *defaultParser) configureFieldPlan(plan mapr.ParserFieldPlan) {
	p.fieldsCapacity = plan.Capacity()
	p.dynamicFields = nil
	p.allDynamicFields = plan.AllFields

	p.wantStar = plan.Needs("*")
	p.wantLine = plan.Needs("$line")
	p.wantEmpty = plan.Needs("$empty")
	p.wantHostname = plan.Needs("$hostname")
	p.wantServer = plan.Needs("$server")
	p.wantTimezone = plan.Needs("$timezone")
	p.wantTimeOffset = plan.Needs("$timeoffset")
	p.wantSeverity = plan.Needs("$severity")
	p.wantLogLevel = plan.Needs("$loglevel")
	p.wantTime = plan.Needs("$time")
	p.wantDate = plan.Needs("$date")
	p.wantHour = plan.Needs("$hour")
	p.wantMinute = plan.Needs("$minute")
	p.wantSecond = plan.Needs("$second")
	p.wantPID = plan.Needs("$pid")
	p.wantCaller = plan.Needs("$caller")
	p.wantCPUs = plan.Needs("$cpus")
	p.wantGoroutines = plan.Needs("$goroutines")
	p.wantCGOCalls = plan.Needs("$cgocalls")
	p.wantLoadAvg = plan.Needs("$loadavg")
	p.wantUptime = plan.Needs("$uptime")

	if plan.AllFields {
		return
	}

	p.dynamicFields = make(map[string]struct{}, len(plan.Fields))
	for field := range plan.Fields {
		p.dynamicFields[field] = struct{}{}
	}
}

// Probe is the default format's own probe: the generic probe, with the fields
// of every line grouped by the MAPREDUCE:<TABLE> of the line, the table a
// query names with `from TABLE`. A table no table name can select
// (mapr.SelectableTable: a name with lower-case letters, as a query's table
// name is upper-cased) is not listed, so that every listed table is one a
// query can name. Its lines are still read by a query without a table (no
// from clause, or `from .`), so their fields and $variables are listed
// under ProbeAnyTable, the entry of no table, which a completion offers
// only while the query names none; the result's Note names the first such
// table.
func (p *defaultParser) Probe(sample ProbeSample) (ProbeResult, error) {
	unselectable := ""
	result, err := probeLines(p, sample, func(line string) string {
		table := defaultLineTable(line)
		if mapr.SelectableTable(table) {
			return table
		}
		if unselectable == "" {
			unselectable = strings.Clone(table)
		}
		return ProbeAnyTable
	})
	if err == nil && unselectable != "" {
		result.Note = unselectableTableNote(unselectable)
	}
	return result, err
}

// unselectableTableNote is the probe note about the lines of table, whose
// fields the probe lists under no table. The name, log content, is cut to
// ProbeMaxNameBytes and Go-quoted.
func unselectableTableNote(table string) string {
	return fmt.Sprintf("the fields of the lines of table %q are listed without a table: table "+
		"names are upper-cased (`from stats` reads STATS), so no table name selects them; "+
		"query them without from (or from .)",
		truncateUTF8(table, ProbeMaxNameBytes))
}

// ProbeLineTable tells the table of a line of the default format without
// parsing its fields (ProbeTabler): like MakeFieldsInto it needs an INFO
// line of at least the 11 tokens of a mapreduce line whose table token
// starts with "MAPREDUCE:". A line whose key=value fields do not parse is
// named a table too, which the probe then lists without fields. A line of a
// table no table name can select (mapr.SelectableTable) is assigned to
// ProbeAnyTable, as Probe lists its fields there: its table is not listed,
// and the table probe of the upper-cased name does not keep it.
func (p *defaultParser) ProbeLineTable(maprLine string) (string, bool) {
	if !strings.HasPrefix(maprLine, "INFO") {
		return "", false
	}
	start, table := 0, ""
	for tokenIndex := 0; ; tokenIndex++ {
		token, next, done := protocol.ScanField(maprLine, start)
		if tokenIndex == defaultTableToken {
			var ok bool
			if table, ok = strings.CutPrefix(token, "MAPREDUCE:"); !ok {
				return "", false
			}
			if !mapr.SelectableTable(table) {
				table = ProbeAnyTable
			}
		}
		if tokenIndex == defaultTableToken+1 {
			return table, true
		}
		if done {
			return "", false
		}
		start = next
	}
}

// defaultLineTable returns the table name of a line of the default format,
// the part after "MAPREDUCE:" of its table token. It is called for lines the
// parser accepted only, whose table token always carries the prefix.
func defaultLineTable(maprLine string) string {
	start := 0
	for tokenIndex := 0; ; tokenIndex++ {
		token, next, done := protocol.ScanField(maprLine, start)
		if tokenIndex == defaultTableToken {
			return strings.TrimPrefix(token, "MAPREDUCE:")
		}
		if done {
			return ProbeAnyTable
		}
		start = next
	}
}
