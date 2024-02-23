package mapr

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
		return "Field"
	case String:
		return "String"
	case Float:
		return "Float"
	case FunctionStack:
		return "FunctionStack"
	default:
		return "UndefFieldType"
	}
}
