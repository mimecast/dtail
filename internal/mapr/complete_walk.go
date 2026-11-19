package mapr

import (
	"slices"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/mapr/funcs"
)

// partialWalker follows the clauses of a query token by token with the
// per-clause rules of Query.parseTokens. For every token it records the
// pieces with their kinds (spans), and at any point it can tell what may
// follow (expect). Like the parser, a clause keyword starts a new clause
// whatever came before, and a later clause of a kind replaces an earlier
// one.
type partialWalker struct {
	spans  []PartialToken
	clause string
	// n counts the tokens of the clause, the optional lead word (by,
	// append) and the joining words of conditions (and, ',') excluded.
	n int
	// lead tells that the optional lead word by (group, order, rorder) or
	// append (outfile) was consumed.
	lead bool
	// cond is the position (0-2) in the current where or set condition,
	// conds the number of complete conditions, and joined tells that the
	// last complete condition is followed by its joining word.
	cond   int
	conds  int
	joined bool
	// left is the index of the span of the current where condition's left
	// operand (re-kinded once the operator is known), leftToken its token;
	// field and op are the left operand and the operator.
	left      int
	leftToken token
	field     string
	op        WhereOperator
	// selectItems are the FieldStorage forms of the last select clause.
	selectItems []string
	table       string
	logFormat   string
}

// step classifies the next token and advances the clause state.
func (w *partialWalker) step(st scannedToken) {
	if st.isKeyword() {
		w.startClause(strings.ToLower(st.str))
		w.add(st.start, st.end, TokenKeyword)
		return
	}
	t := stripBackticks(st.token)
	switch w.clause {
	case "":
		// The parser rejects anything before the first clause keyword.
		w.add(st.start, st.end, TokenUnknown)
	case "select":
		w.stepSelect(st, t)
	case "from":
		w.stepSingle(st, w.n == 0 && t.str != "", TokenTable)
		if w.n == 1 && t.str != "" {
			w.table = TableName(t.str)
		}
	case "logformat":
		w.stepSingle(st, w.n == 0 && t.str != "", TokenLogFormat)
		if w.n == 1 && t.str != "" {
			w.logFormat = t.str
		}
	case "interval", "limit":
		w.stepSingle(st, w.n == 0 && isClauseNumber(w.clause, t.str), TokenNumber)
	case "where":
		w.stepWhere(st, t)
	case "set":
		w.stepSet(st, t)
	case "group", "order", "rorder", "outfile":
		w.stepLeadClause(st, t)
	default:
		// A clause keyword without a case here (see expect).
		w.add(st.start, st.end, TokenUnknown)
	}
}

// expect returns the kinds of words that may follow the tokens so far.
func (w *partialWalker) expect() Expect {
	switch w.clause {
	case "":
		return ExpectClauseKeyword
	case "select":
		if w.n == 0 {
			return ExpectField | ExpectAggregation
		}
		return ExpectField | ExpectAggregation | ExpectClauseKeyword
	case "from":
		return w.expectSingle(ExpectTable)
	case "logformat":
		return w.expectSingle(ExpectLogFormat)
	case "interval", "limit":
		return w.expectSingle(ExpectNumber)
	case "where":
		return w.expectWhere()
	case "set":
		return w.expectSet()
	case "group":
		return w.expectLead(ExpectBy, ExpectField, ExpectField|ExpectClauseKeyword)
	case "order", "rorder":
		return w.expectLead(ExpectBy, ExpectSelectItem, ExpectClauseKeyword)
	case "outfile":
		return w.expectLead(ExpectAppend, ExpectOutfilePath, ExpectClauseKeyword)
	default:
		// A clause keyword without a case here: nothing known can follow.
		// TestAnalyzePartialKnowsEveryClause fails for such a keyword.
		return 0
	}
}

// snapshot stores the state before the word under the cursor in p.
func (w *partialWalker) snapshot(p *Partial) {
	p.Clause = w.clause
	p.Expect = w.expect()
	p.SelectItems = slices.Clone(w.selectItems)
	p.Table = w.table
	p.LogFormat = w.logFormat
	if w.clause == "where" && w.cond == 2 {
		p.ValueField = w.field
		p.ValueOperator = w.op
	}
}

// startClause resets the clause state for the clause of keyword.
func (w *partialWalker) startClause(keyword string) {
	w.clause = keyword
	w.n, w.lead = 0, false
	w.cond, w.conds, w.joined = 0, 0, false
	if keyword == "select" {
		w.selectItems = nil
	}
}

// add records a piece of the query.
func (w *partialWalker) add(start, end int, kind TokenKind) {
	w.spans = append(w.spans, PartialToken{Start: start, End: end, Kind: kind})
}

// stepSingle steps a clause of one value (from, logformat, interval,
// limit): the parser uses the first token and rejects more
// (strayWordError).
func (w *partialWalker) stepSingle(st scannedToken, valid bool, kind TokenKind) {
	if !valid {
		kind = TokenUnknown
	}
	w.n++
	w.add(st.start, st.end, kind)
}

func (w *partialWalker) expectSingle(value Expect) Expect {
	if w.n == 0 {
		return value
	}
	return ExpectClauseKeyword
}

// stepSelect steps a select item (parseSelectCondition). A bare by is
// rejected by the parser (rejectStrayBy) and no select item.
func (w *partialWalker) stepSelect(st scannedToken, t token) {
	w.n++
	if isBareWord(t, "by") {
		w.add(st.start, st.end, TokenUnknown)
		return
	}
	if sc, err := parseSelectCondition(t); err == nil {
		w.selectItems = append(w.selectItems, sc.FieldStorage)
	}
	w.addSelectItem(st, t)
}

// addSelectItem records a select item, or the item of an order clause, by
// the rule of parseSelectCondition: a valid aggregation as its name and its
// field, anything else as one piece. An item the parser rejects, such as an
// unknown aggregation, count(foo)bar or an aggregation still being typed
// (count(fo), is TokenUnknown as a whole.
func (w *partialWalker) addSelectItem(st scannedToken, t token) {
	sc, err := parseSelectCondition(t)
	switch {
	case err != nil:
		w.add(st.start, st.end, TokenUnknown)
	case sc.FieldStorage == sc.Field:
		w.add(st.start, st.end, nameKind(sc.Field))
	case !st.isBareword:
		// A quoted aggregation ("count(foo)") is one piece.
		w.add(st.start, st.end, TokenAggregation)
	default:
		open := strings.IndexByte(t.str, '(')
		w.add(st.start, st.start+open, TokenAggregation)
		if sc.Field != "" {
			start := st.start + open + 1
			w.add(start, start+len(sc.Field), nameKind(sc.Field))
		}
	}
}

// stepLeadClause steps the clauses with an optional lead word: group and
// (r)order (by, matched on the raw token before backticks are stripped and
// ignoring case, as tokensConsumeOptional does) and outfile (the bare lower
// case word append, isAppendWord, as parseOutfileClause: a quoted or
// escaped append is the path).
func (w *partialWalker) stepLeadClause(st scannedToken, t token) {
	isLead := strings.EqualFold(st.str, "by")
	if w.clause == "outfile" {
		isLead = isAppendWord(t)
	}
	switch {
	case w.n == 0 && !w.lead && isLead:
		w.lead = true
		w.add(st.start, st.end, TokenKeyword)
		return
	case w.clause == "group" && isBareWord(t, "by"):
		// A bare by after the lead word or a field (rejectStrayBy).
		w.add(st.start, st.end, TokenUnknown)
	case w.clause == "group":
		w.add(st.start, st.end, nameKind(t.str))
	case (w.clause == "order" || w.clause == "rorder") && w.n == 1 &&
		isBareWord(t, orderDirection(w.clause == "rorder")):
		// The direction word parseOrderClause allows right after the field
		// of order (desc) and rorder (asc); a word after it is unknown.
		w.add(st.start, st.end, TokenKeyword)
	case w.n > 0:
		// The parser rejects more tokens (strayWordError, orderWordError).
		w.add(st.start, st.end, TokenUnknown)
	case w.clause == "outfile":
		w.add(st.start, st.end, outfilePathKind(t.str))
	default:
		w.addSelectItem(st, t)
	}
	w.n++
}

// expectLead returns the expectation of a clause with an optional lead
// word: the lead word or the first value, the first value after the lead
// word, then next (more values or the end of the clause).
func (w *partialWalker) expectLead(lead, value, next Expect) Expect {
	switch {
	case w.n > 0:
		return next
	case w.lead:
		return value
	default:
		return lead | value
	}
}

// stepWhere steps a where condition (makeWhereConditions): left operand,
// operator, right operand, then an optional and.
func (w *partialWalker) stepWhere(st scannedToken, t token) {
	switch w.cond {
	case 0:
		if w.conds > 0 && !w.joined && strings.EqualFold(t.str, "and") {
			w.joined = true
			w.add(st.start, st.end, TokenKeyword)
			return
		}
		w.joined = false
		w.left, w.leftToken, w.field = len(w.spans), t, ""
		if t.isBareword {
			// A quoted left operand is a literal, not a field.
			w.field = t.str
		}
		w.op = WhereOperator{}
		w.add(st.start, st.end, operandKind(t, false))
	case 1:
		kind := TokenUnknown
		if op, err := parseWhereOperation(t.str); err == nil {
			w.op = WhereOperator{Name: strings.ToLower(t.str), Kind: op.kind()}
			kind = TokenOperator
			w.spans[w.left].Kind = operandKind(w.leftToken, w.isNumeric())
		}
		w.add(st.start, st.end, kind)
	default:
		w.add(st.start, st.end, operandKind(t, w.isNumeric()))
		w.conds++
	}
	w.cond = (w.cond + 1) % 3
}

func (w *partialWalker) isNumeric() bool {
	return w.op.Name != "" && w.op.Kind == NumericOperator
}

func (w *partialWalker) expectWhere() Expect {
	switch w.cond {
	case 1:
		return ExpectWhereOperator
	case 2:
		return ExpectValue | ExpectField
	}
	if w.conds > 0 && !w.joined {
		return ExpectField | ExpectAnd | ExpectClauseKeyword
	}
	return ExpectField
}

// stepSet steps a set condition (makeSetConditions): $variable, =, value,
// then an optional ',' (only a quoted one survives the tokenizer).
func (w *partialWalker) stepSet(st scannedToken, t token) {
	kind := TokenUnknown
	switch w.cond {
	case 0:
		if w.conds > 0 && !w.joined && t.str == "," {
			w.joined = true
			w.add(st.start, st.end, TokenOperator)
			return
		}
		w.joined = false
		if t.isBareword && strings.HasPrefix(t.str, "$") {
			kind = TokenVariable
		}
	case 1:
		if t.str == "=" {
			kind = TokenOperator
		}
	default:
		w.addSetValue(st, t)
		w.conds++
		w.cond = 0
		return
	}
	w.add(st.start, st.end, kind)
	w.cond++
}

func (w *partialWalker) expectSet() Expect {
	switch w.cond {
	case 1:
		return ExpectAssign
	case 2:
		return ExpectSetFunction | ExpectField
	}
	if w.conds > 0 && !w.joined {
		return ExpectSetVariable | ExpectClauseKeyword
	}
	return ExpectSetVariable
}

// addSetValue records the value of a set condition by the rules of
// makeSetConditions: a quoted string, an escaped field, a function call
// (a bareword ending with ")"), a number or a field.
func (w *partialWalker) addSetValue(st scannedToken, t token) {
	switch {
	case !t.isBareword:
		w.add(st.start, st.end, TokenString)
	case t.quotesStripped:
		w.add(st.start, st.end, nameKind(t.str))
	case strings.HasSuffix(t.str, ")"):
		w.addFunctionCall(st)
	default:
		w.add(st.start, st.end, operandKind(t, true))
	}
}

// addFunctionCall records the function names and the argument of a set
// function call such as md5sum(maskdigits($foo)), as funcs.NewFunctionStack
// parses it; a call it rejects is TokenUnknown as a whole. Each function
// takes its name and "(" from the front and its ")" from the end, so the
// argument starts after the last name.
func (w *partialWalker) addFunctionCall(st scannedToken) {
	stack, arg, err := funcs.NewFunctionStack(st.str)
	if err != nil {
		w.add(st.start, st.end, TokenUnknown)
		return
	}
	offset := st.start
	for _, function := range stack {
		w.add(offset, offset+len(function.Name), TokenFunction)
		offset += len(function.Name) + 1
	}
	if arg != "" {
		w.add(offset, offset+len(arg), operandKind(token{str: arg, isBareword: true}, true))
	}
}

// operandKind classifies an operand of a where condition (whereCondition.fill)
// or the value of a set condition: a quoted string, a number, or a field. A
// numeric operator takes every operand that parses as a float as a number,
// backticks or not; a string operator only a plain decimal number
// (stringOperandType). A quoted operand of a numeric operator is rejected.
func operandKind(t token, numeric bool) TokenKind {
	switch {
	case !t.isBareword && numeric:
		// A numeric operator needs barewords on both sides.
		return TokenUnknown
	case !t.isBareword:
		return TokenString
	}
	if numeric {
		if _, err := strconv.ParseFloat(t.str, 64); err == nil {
			return TokenNumber
		}
	} else if !t.quotesStripped && isNumericLiteral(t.str) {
		return TokenNumber
	}
	return nameKind(t.str)
}

// isClauseNumber tells whether str is a number the interval
// (parseIntervalClause) or limit (parseLimitClause) clause accepts.
func isClauseNumber(clause, str string) bool {
	n, err := strconv.Atoi(str)
	if err != nil {
		return false
	}
	if clause == "interval" {
		return validInterval(n)
	}
	return n >= 0
}

// nameKind classifies a field name: a $variable or a field.
func nameKind(name string) TokenKind {
	switch {
	case name == "":
		return TokenUnknown
	case strings.HasPrefix(name, "$"):
		return TokenVariable
	default:
		return TokenField
	}
}

// outfilePathKind classifies an outfile path by the rules of
// parseOutfileClause: it must not be empty and must not start with
// "append:" (isAppendColonPath), with or without the append word before it.
func outfilePathKind(path string) TokenKind {
	if path == "" || isAppendColonPath(path) {
		return TokenUnknown
	}
	return TokenString
}
