package loggers

import (
	"strings"
	"testing"
)

type rotatingRecordingLogger struct {
	recordingSink
	rotations int
}

func (r *rotatingRecordingLogger) Rotate() { r.rotations++ }

func TestFactoryReturnsErrorForUnsupportedLogger(t *testing.T) {
	_, err := Factory("test", "carrier-pigeon", Strategy{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("Factory error = %v, want unsupported logger name", err)
	}
}

func TestFactoryNormalizesRegisteredLoggerName(t *testing.T) {
	logger, err := Factory(t.Name(), "NoNe", Strategy{}, Options{})
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if _, ok := logger.(none); !ok {
		t.Fatalf("Factory returned %T, want none", logger)
	}
}

func TestFactoryRotateUsesOptionalRotator(t *testing.T) {
	rotating := &rotatingRecordingLogger{}

	factoryMutex.Lock()
	previous := factoryMap
	factoryMap = map[string]Logger{
		"core-only": &recordingSink{},
		"rotating":  rotating,
	}
	factoryMutex.Unlock()
	t.Cleanup(func() {
		factoryMutex.Lock()
		factoryMap = previous
		factoryMutex.Unlock()
	})

	FactoryRotate()
	if rotating.rotations != 1 {
		t.Fatalf("rotations = %d, want 1", rotating.rotations)
	}
}
