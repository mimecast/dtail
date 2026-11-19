package sessionuser

import "testing"

func TestSplitPermissionType(t *testing.T) {
	tests := []struct{ rule, wantType, wantRule string }{
		{"readfiles:^/var/log/.*$", "readfiles", "^/var/log/.*$"},
		{"readfiles:!^journal:sshd$", "readfiles", "!^journal:sshd$"},
		{"runcommands:^/bin/.*", "runcommands", "^/bin/.*"},
		{"^/var/log/.*$", "readfiles", "^/var/log/.*$"},
		{"!^/var/log/(?:secret|audit)/", "readfiles", "!^/var/log/(?:secret|audit)/"},
		{"!^journal:sshd\\.service$", "readfiles", "!^journal:sshd\\.service$"},
		{"^/var/log/[[:alpha:]]+\\.key$", "readfiles", "^/var/log/[[:alpha:]]+\\.key$"},
	}
	for _, tc := range tests {
		gotType, gotRule := splitPermissionType(tc.rule)
		if gotType != tc.wantType || gotRule != tc.wantRule {
			t.Errorf("splitPermissionType(%q) = %q, %q; want %q, %q",
				tc.rule, gotType, gotRule, tc.wantType, tc.wantRule)
		}
	}
}
