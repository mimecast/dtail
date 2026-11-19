package fs

import (
	"fmt"
	"strings"
)

// journalUnitNameMax is the longest unit name systemd accepts (UNIT_NAME_MAX
// of 256 bytes includes the terminating NUL).
const journalUnitNameMax = 255

// journalUnitTypes are the unit type suffixes a journal: target may end in:
// systemd's unit types (systemd.unit(5)) except "slice", as journalctl -u of
// a slice also reads every unit in it (it matches _SYSTEMD_SLICE), which no
// readfiles rule on the slice's name covers.
var journalUnitTypes = map[string]struct{}{
	"service": {}, "socket": {}, "device": {}, "mount": {}, "automount": {},
	"swap": {}, "target": {}, "path": {}, "timer": {}, "scope": {},
}

// validateJournalSpec checks that the unit of a journal: target is one
// literal systemd unit name, the one journalctl -u reads. The readfiles
// permission check matches the target string, so every name journalctl would
// widen must be refused:
//   - journalctl -u treats a unit containing '*', '?' or '[' as a glob over
//     the journal's units ("ssh*.service" reads sshd.service);
//   - it appends ".service" to a name without a valid unit type ("sshd");
//   - a slice reads all units in it.
//
// So the unit must be a plain or instance unit name as systemd validates it
// (unit_name_is_valid): at most 255 bytes of ASCII letters, digits and
// ":-_.\@", a non-empty prefix before the first '@' and the last '.', a
// non-empty instance after an '@' (a template "getty@.service" is refused)
// and a unit type suffix other than "slice". Such a name journalctl uses
// verbatim. A leading '-' is refused as well, so it can never be read as an
// option. Being plain ASCII, the name contains no whitespace or control
// character.
func validateJournalSpec(spec string) error {
	unit := strings.TrimPrefix(spec, JournalSpecPrefix)
	switch {
	case unit == "":
		return fmt.Errorf("journal read target requires a unit name after %q", JournalSpecPrefix)
	case len(unit) > journalUnitNameMax:
		return fmt.Errorf("journal read target unit is longer than %d bytes", journalUnitNameMax)
	case strings.HasPrefix(unit, "-"):
		return fmt.Errorf("journal read target unit must not start with '-'")
	}
	for i := 0; i < len(unit); i++ {
		if !isJournalUnitNameByte(unit[i]) {
			return fmt.Errorf("journal read target unit %q contains %q, "+
				"which is no character of a literal systemd unit name", unit, unit[i])
		}
	}
	return validateJournalUnitParts(unit)
}

// validateJournalUnitParts checks the structure of a unit name made of valid
// bytes: "prefix[@instance].type" with a known, readable unit type.
func validateJournalUnitParts(unit string) error {
	dot := strings.LastIndexByte(unit, '.')
	if dot <= 0 {
		return fmt.Errorf("journal read target unit %q needs a unit name and a unit type suffix "+
			"such as .service", unit)
	}
	unitType := unit[dot+1:]
	if unitType == "slice" {
		return fmt.Errorf("journal read target unit %q is a slice, which names the units in it", unit)
	}
	if _, ok := journalUnitTypes[unitType]; !ok {
		return fmt.Errorf("journal read target unit %q has no unit type suffix such as .service", unit)
	}
	at := strings.IndexByte(unit[:dot], '@')
	switch {
	case at == 0:
		return fmt.Errorf("journal read target unit %q has an empty name before '@'", unit)
	case at > 0 && at == dot-1:
		return fmt.Errorf("journal read target unit %q is a template, not a unit", unit)
	}
	return nil
}

// isJournalUnitNameByte reports whether c may appear in a systemd unit name
// (VALID_CHARS_WITH_AT of systemd's unit-name.c).
func isJournalUnitNameByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte(":-_.\\@", c) >= 0
}
