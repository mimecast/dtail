package loggers

import (
	"fmt"
	"strings"
	"sync"
)

var factoryMap map[string]Logger
var factoryMutex sync.Mutex

func Factory(sourceName, loggerName string, rotationStrategy Strategy) Logger {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()

	id := fmt.Sprintf("sourceName:%s,fileBase:%s,loggerName:%s", sourceName, rotationStrategy.FileBase, loggerName)
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
			singleton = newFile(rotationStrategy)
			factoryMap[id] = singleton
		case "fout":
			singleton = newFout(rotationStrategy)
			factoryMap[id] = singleton
		default:
			panic(fmt.Sprintf("Unsupported logger type '%s'", loggerName))
		}
	}

	return singleton
}

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
