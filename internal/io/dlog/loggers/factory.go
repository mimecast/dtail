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

func Factory(name string, impl Impl, strategy Strategy) Logger {
	factoryMutex.Lock()
	defer factoryMutex.Unlock()

	id := fmt.Sprintf("name:%s,fileBase:%s,impl:%v", name, strategy.FileBase, impl)

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
			singleton = newFile(strategy)
			factoryMap[id] = singleton
		case FOUT:
			singleton = newFout(strategy)
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
