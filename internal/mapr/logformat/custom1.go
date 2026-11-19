package logformat

import "errors"

// ErrCustom1NotImplemented indicates custom1 parser is only a template.
var ErrCustom1NotImplemented error = errors.New("custom1 log format is not implemented")

// Template for creating a custom log format.
type custom1Parser struct{}

var _ Parser = (*custom1Parser)(nil)

func newCustom1Parser(hostname, timeZoneName string, timeZoneOffset int) (*custom1Parser, error) {
	return &custom1Parser{}, ErrCustom1NotImplemented
}

func (p *custom1Parser) MakeFields(maprLine, _ string) (map[string]string, error) {
	return nil, ErrCustom1NotImplemented
}

// A format needs no probe of its own: Probe runs ProbeWithParser, which parses
// the sampled lines with MakeFields above, when the parser does not implement
// Prober. To name tables, list a fixed schema, hide fields or supply better
// sample values, implement Prober, e.g. (see doc/logformats.md, "Custom
// probes"):
//
//	var _ Prober = (*custom1Parser)(nil)
//
//	// Probe lists the fields the generic probe found under the table ORDERS,
//	// hides the internal field "secret" and adds a note for `:fields`.
//	func (p *custom1Parser) Probe(sample ProbeSample) (ProbeResult, error) {
//		result, err := ProbeWithParser(p, sample)
//		if err != nil {
//			return result, err
//		}
//		if fields, ok := result.Tables[ProbeAnyTable]; ok {
//			delete(fields, "secret")
//			delete(result.Tables, ProbeAnyTable)
//			result.Tables["ORDERS"] = fields
//			// A note alone makes a result non-empty, so set it only when
//			// the sample matched the format.
//			result.Note = "custom1: order log, one line per order"
//		}
//		return result, nil
//	}
