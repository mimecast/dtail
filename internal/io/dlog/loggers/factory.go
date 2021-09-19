package loggers

import (
	"fmt"
	"sync"
)

type Impl int

const (
	NONE   Impl = iota
	STDOUT Impl = iota
	FILE   Impl = iota
	FOUT   Impl = iota
)

var factoryMap map[string]Logger
var factoryMutex sync.Mutex

func Factory(name string, impl Impl) Logger {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()

	id := fmt.Sprintf("name:%s,impl:%v", name, impl)

	if factoryMap == nil {
		factoryMap = make(map[string]Logger)
	}

	singleton, ok := factoryMap[id]
	if !ok {
		switch impl {
		case NONE:
			singleton = none{}
		case STDOUT:
			singleton = newStdout()
			factoryMap[id] = singleton
		case FILE:
			singleton = newFile()
			factoryMap[id] = singleton
		case FOUT:
			singleton = newFout()
			factoryMap[id] = singleton
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

	for _, impl := range factoryMap {
		impl.Rotate()
	}
}
