package loggers

import (
	"fmt"
	"strings"
	"sync"
)

var factoryMap map[string]Logger
var factoryMutex sync.Mutex

// Factory is there to retrieve a logger based on various settings.
func Factory(sourceName, loggerName string, logRotation Strategy) Logger {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()

	id := fmt.Sprintf("sourceName:%s,fileBase:%s,loggerName:%s", sourceName,
		logRotation.FileBase, loggerName)
	if factoryMap == nil {
		factoryMap = make(map[string]Logger)
	}

	singleton, ok := factoryMap[id]
	if !ok {
		switch strings.ToLower(loggerName) {
		case "none":
			singleton = none{}
		case "stdout":
			singleton = newStdout()
			factoryMap[id] = singleton
		case "file":
			singleton = newFile(logRotation)
			factoryMap[id] = singleton
		case "fout":
			singleton = newFout(logRotation)
			factoryMap[id] = singleton
		default:
			panic(fmt.Sprintf("Unsupported logger type '%s'", loggerName))
		}
	}
	return singleton
}

// FactoryRotate invokes a log rotation of all loggers.
func FactoryRotate() {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()
	if factoryMap == nil {
		return
	}
	for _, logger := range factoryMap {
		logger.Rotate()
	}
}
