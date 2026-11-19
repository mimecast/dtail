package loggers

import (
	"fmt"
	"strings"
	"sync"
)

var factoryMap map[string]Logger
var factoryMutex sync.Mutex

// Options contains process-owned logger settings.
type Options struct {
	LogDir     string
	LogPayload bool
}

type loggerConstructor func(Strategy, Options) Logger

var loggerRegistry = map[string]loggerConstructor{
	"none": func(Strategy, Options) Logger { return none{} },
	"stdout": func(Strategy, Options) Logger {
		return newStdout()
	},
	"file": func(strategy Strategy, options Options) Logger {
		return newFile(strategy, options.LogDir)
	},
	"fout": func(strategy Strategy, options Options) Logger {
		return newFout(strategy, options)
	},
}

// Factory is there to retrieve a logger based on various settings.
func Factory(sourceName, loggerName string, logRotation Strategy, options Options) (Logger, error) {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()

	loggerName = strings.ToLower(loggerName)
	constructor, ok := loggerRegistry[loggerName]
	if !ok {
		return nil, fmt.Errorf("unsupported logger type %q", loggerName)
	}
	id := fmt.Sprintf("sourceName:%s,fileBase:%s,loggerName:%s,logDir:%s,logPayload:%t", sourceName,
		logRotation.FileBase, loggerName, options.LogDir, options.LogPayload)
	if factoryMap == nil {
		factoryMap = make(map[string]Logger)
	}

	singleton, ok := factoryMap[id]
	if !ok {
		singleton = constructor(logRotation, options)
		factoryMap[id] = singleton
	}
	return singleton, nil
}

// FactoryRotate invokes a log rotation of all loggers.
func FactoryRotate() {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()
	if factoryMap == nil {
		return
	}
	for _, logger := range factoryMap {
		if rotator, ok := logger.(Rotator); ok {
			rotator.Rotate()
		}
	}
}
