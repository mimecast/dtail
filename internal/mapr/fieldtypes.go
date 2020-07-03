package mapr

import "fmt"

type fieldType int

// The possible field types.
const (
	UndefFieldType fieldType = iota
	Field          fieldType = iota
	String         fieldType = iota
	Float          fieldType = iota
	FunctionStack  fieldType = iota
)

func (w fieldType) String() string {
	switch w {
	case Field:
		return fmt.Sprintf("Field")
	case String:
		return fmt.Sprintf("String")
	case Float:
		return fmt.Sprintf("Float")
	case FunctionStack:
		return fmt.Sprintf("FunctionStack")
	default:
		return fmt.Sprintf("UndefFieldType")
	}
}
