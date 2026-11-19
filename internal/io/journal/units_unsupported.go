//go:build !linux

package journal

import "context"

// ListUnits lists no units outside Linux; see units.go. Servers there never
// advertise the journal capability, so the probe never asks.
func ListUnits(_ context.Context, _ int) ([]string, bool, error) {
	return nil, false, ErrUnsupported
}
