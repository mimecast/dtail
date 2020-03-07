package logformat

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/mimecast/dtail/internal/io/logger"
)

// Parser is used to parse the mapreduce information from the server log files.
type Parser struct {
	hostname           string
	logFormatName      string
	makeFieldsFunc     reflect.Value
	makeFieldsReceiver reflect.Value
}

// NewParser returns a new log parser.
func NewParser(logFormatName string) (*Parser, error) {
	hostname, err := os.Hostname()

	if err != nil {
		return nil, err
	}

	p := Parser{
		hostname: hostname,
	}

	err = p.reflectLogFormat(logFormatName)
	if err != nil {
		return nil, err
	}

	return &p, nil
}

// The aim of this is that everyone can plug in their own mapr log format
// parsing method to DTail. Just add a method MakeFieldsMODULENAME to type
// Parser. Whereas MODULENAME must be a upeprcase string.
func (p *Parser) reflectLogFormat(logFormatName string) error {
	methodName := fmt.Sprintf("MakeFields%s", strings.ToUpper(logFormatName))

	rt := reflect.TypeOf(p)
	method, ok := rt.MethodByName(methodName)
	if !ok {
		return errors.New("No such mapr log format module: " + methodName)
	}

	p.makeFieldsFunc = method.Func
	p.makeFieldsReceiver = reflect.ValueOf(p)

	return nil
}

// MakeFields is for returning the fields from a given log line.
func (p *Parser) MakeFields(maprLine string) (fields map[string]string, err error) {
	inputValues := []reflect.Value{p.makeFieldsReceiver, reflect.ValueOf(maprLine)}
	returnValues := p.makeFieldsFunc.Call(inputValues)

	errInterface := returnValues[1].Interface()

	if errInterface == nil {
		fields, err = returnValues[0].Interface().(map[string]string), nil
		logger.Trace("parser.MakeFields", fields, err)
		return
	}

	fields, err = returnValues[0].Interface().(map[string]string), errInterface.(error)
	logger.Trace("parser.MakeFields", fields, err)

	return
}
