package mapr

// SetClause interprets the set clause of the mapreduce query.
func (q *Query) SetClause(fields map[string]string) error {
	for _, sc := range q.Set {
		value, ok := fields[sc.rString]
		if !ok {
			continue
		}

		switch sc.rType {
		case FunctionStack:
			fields[sc.lString] = sc.functionStack.Call(value)
		default:
			fields[sc.lString] = value
		}
	}

	return nil
}
