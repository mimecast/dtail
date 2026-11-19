package mapr

// ParserFieldPlan describes which raw fields a parser needs to materialize.
type ParserFieldPlan struct {
	AllFields bool
	Fields    map[string]struct{}
}

// Needs reports whether the parser plan requires a field.
func (p ParserFieldPlan) Needs(field string) bool {
	if p.AllFields {
		return true
	}
	_, ok := p.Fields[field]
	return ok
}

// Capacity returns a reasonable initial capacity for a parsed field map.
func (p ParserFieldPlan) Capacity() int {
	if p.AllFields {
		return 20
	}
	if len(p.Fields) == 0 {
		return 4
	}
	return len(p.Fields) + 2
}

// ParserFieldPlan returns the raw fields required to evaluate the query.
func (q *Query) ParserFieldPlan() ParserFieldPlan {
	if q == nil {
		return ParserFieldPlan{AllFields: true}
	}

	fields := make(map[string]struct{}, len(q.Select)+len(q.GroupBy)+len(q.Where)*2+len(q.Set))
	producedBySet := make(map[string]struct{}, len(q.Set))

	add := func(field string) {
		if field == "" {
			return
		}
		fields[field] = struct{}{}
	}
	isProduced := func(field string) bool {
		_, ok := producedBySet[field]
		return ok
	}

	for _, wc := range q.Where {
		if wc.lType == Field {
			add(wc.lString)
		}
		if wc.rType == Field {
			add(wc.rString)
		}
	}

	for _, sc := range q.Set {
		switch sc.rType {
		case Field, FunctionStack:
			if !isProduced(sc.rString) {
				add(sc.rString)
			}
		}
		producedBySet[sc.lString] = struct{}{}
	}

	for _, groupBy := range q.GroupBy {
		if !isProduced(groupBy) {
			add(groupBy)
		}
	}

	for _, sc := range q.Select {
		if !isProduced(sc.Field) {
			add(sc.Field)
		}
	}

	return ParserFieldPlan{Fields: fields}
}
