package journal

import (
	"reflect"
	"testing"
)

func TestUnitArgs(t *testing.T) {
	tests := []struct {
		spec string
		want []string
	}{
		{spec: "journal:ssh.service", want: []string{"-u", "ssh.service"}},
		{spec: "journal:", want: nil},
	}
	for _, tt := range tests {
		if got := UnitArgs(tt.spec); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("UnitArgs(%q) = %q, want %q", tt.spec, got, tt.want)
		}
	}
}
