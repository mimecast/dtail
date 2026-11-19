package clients

import (
	"testing"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// A connection that reconnected starts its session over at generation 1
// while the others keep theirs. A reload must still bring every connection
// to the same generation instead of failing with mismatched generations.
func TestInteractiveReloadAfterReconnectAlignsGenerations(t *testing.T) {
	oldSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	connA := &interactiveReloadConnector{server: "srv1", supported: true,
		committedSpec: oldSpec, liveSpec: oldSpec, generation: 4}
	// srv2 reconnected: its SESSION START was acknowledged with generation 1.
	connB := &interactiveReloadConnector{server: "srv2", supported: true,
		committedSpec: oldSpec, liveSpec: oldSpec, generation: 1}
	recorder := &interactiveReloadRecorder{}

	client := &baseClient{
		mu:          newBaseClientMu(),
		sessionSpec: oldSpec,
		connections: []connectors.Connector{connA, connB},
		profile:     clientProfile{commit: recorder.commitSessionSpec},
	}

	nextArgs := config.Args{Mode: omode.GrepClient, What: "/tmp/new.log", RegexStr: "WARN"}
	nextSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/tmp/new.log"}, Regex: "WARN"}
	for i := 0; i < 2; i++ {
		if err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
			t.Fatalf("reload %d: %v", i+1, err)
		}
		_, genA, _ := connA.CommittedSession()
		_, genB, _ := connB.CommittedSession()
		if want := uint64(5 + i); genA != want || genB != want {
			t.Fatalf("reload %d: generations %d and %d, want %d", i+1, genA, genB, want)
		}
	}
}
