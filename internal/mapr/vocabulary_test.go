package mapr

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr/funcs"
)

// keywordQueries holds a minimal query for every clause keyword. A keyword
// added to the table without an entry here fails TestKeywordsParse, so every
// keyword the completer offers is known to start a clause the parser handles.
var keywordQueries = map[string]string{
	"select":    "select foo",
	"from":      "select foo from bar",
	"where":     "select foo where foo eq 1",
	"set":       "select foo set $a = 1",
	"group":     "select foo group by foo",
	"rorder":    "select foo rorder by foo",
	"order":     "select foo order by foo",
	"interval":  "select foo interval 5",
	"limit":     "select foo limit 5",
	"outfile":   "select foo outfile \"result.csv\"",
	"logformat": "select foo logformat generic",
}

func TestKeywordsParse(t *testing.T) {
	keywords := Keywords()
	if len(keywords) != len(keywordQueries) {
		t.Errorf("Keywords() = %v, want the %d keywords of keywordQueries",
			keywords, len(keywordQueries))
	}
	for _, keyword := range keywords {
		queryStr, ok := keywordQueries[keyword]
		if !ok {
			t.Errorf("keyword %q has no minimal query in keywordQueries", keyword)
			continue
		}
		for _, q := range []string{queryStr, strings.ToUpper(queryStr)} {
			if _, err := NewQuery(q, logging.NopLogger{}); err != nil {
				t.Errorf("NewQuery(%q) for keyword %q: %v", q, keyword, err)
			}
		}
		if !(token{str: strings.ToUpper(keyword), isBareword: true}).isKeyword() {
			t.Errorf("upper case %q is no keyword", keyword)
		}
	}
}

func TestKeywordNegatives(t *testing.T) {
	for _, tok := range []token{
		{str: "selects", isBareword: true},
		{str: "by", isBareword: true},
		{str: "and", isBareword: true},
		{str: "select", isBareword: false}, // a quoted "select"
	} {
		if tok.isKeyword() {
			t.Errorf("token %#v is a keyword, want none", tok)
		}
	}
	// "frm" is no keyword: it does not start a from clause but is taken as
	// two more select fields, so the query has no table.
	q, err := NewQuery("select foo frm bar", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery with a misspelled keyword: %v", err)
	}
	if q.Table != "" || len(q.Select) != 3 || q.Select[1].Field != "frm" {
		t.Errorf("misspelled keyword parsed as a clause: table %q, select %v",
			q.Table, q.Select)
	}
}

// wantAggregations maps every aggregation name to its operation, written
// down independently of aggregationTable so that a wrong mapping fails.
var wantAggregations = map[string]AggregateOperation{
	"count": Count, "sum": Sum, "min": Min, "max": Max, "avg": Avg,
	"last": Last, "len": Len, "percentage": Percentage, "percentile": Percentile,
}

func TestAggregationNamesParse(t *testing.T) {
	names := AggregationNames()
	if len(names) != len(wantAggregations) {
		t.Errorf("AggregationNames() = %v, want the %d of wantAggregations",
			names, len(wantAggregations))
	}
	for _, name := range names {
		queryStr := "select " + name + "(foo)"
		q, err := NewQuery(queryStr, logging.NopLogger{})
		if err != nil {
			t.Errorf("NewQuery(%q): %v", queryStr, err)
			continue
		}
		want, ok := wantAggregations[name]
		if !ok || q.Select[0].Operation != want {
			t.Errorf("%q parsed to operation %v, want %v (listed: %v)", queryStr,
				q.Select[0].Operation, want, ok)
		}
	}
}

func TestAggregationNegatives(t *testing.T) {
	for _, queryStr := range []string{
		"select median(foo)",
		"select COUNT(foo)", // aggregation names are case sensitive
		"select (foo)",
	} {
		if _, err := NewQuery(queryStr, logging.NopLogger{}); err == nil {
			t.Errorf("NewQuery(%q) succeeded, want an unknown aggregation error", queryStr)
		}
	}
}

// wantWhereOperations maps every where operator, aliases included, to its
// operation, written down independently of whereOperatorTable so that a
// wrong mapping fails.
var wantWhereOperations = map[string]QueryOperation{
	"==": FloatEq, "!=": FloatNe, "<": FloatLt, "<=": FloatLe, "=<": FloatLe,
	">": FloatGt, ">=": FloatGe, "=>": FloatGe,
	"eq": StringEq, "ne": StringNe, "contains": StringContains,
	"ncontains": StringNotContains, "lacks": StringNotContains,
	"hasprefix": StringHasPrefix, "nhasprefix": StringNotHasPrefix,
	"hassuffix": StringHasSuffix, "nhassuffix": StringNotHasSuffix,
}

func TestWhereOperatorsParse(t *testing.T) {
	ops := WhereOperators()
	if len(ops) != len(wantWhereOperations) {
		t.Errorf("WhereOperators() = %v, want the %d of wantWhereOperations",
			ops, len(wantWhereOperations))
	}
	for _, op := range ops {
		want, ok := wantWhereOperations[op.Name]
		if !ok {
			t.Errorf("operator %q missing in wantWhereOperations", op.Name)
			continue
		}
		if isNumeric := want > FloatOperation; isNumeric != (op.Kind == NumericOperator) {
			t.Errorf("operator %q has kind %v, want numeric %v", op.Name, op.Kind, isNumeric)
		}
		for _, name := range []string{op.Name, strings.ToUpper(op.Name)} {
			queryStr := "select foo where foo " + name + " 1"
			q, err := NewQuery(queryStr, logging.NopLogger{})
			if err != nil {
				t.Errorf("NewQuery(%q): %v", queryStr, err)
				continue
			}
			if got := q.Where[0].Operation; got != want {
				t.Errorf("%q parsed to operation %v, want %v", queryStr, got, want)
			}
		}
	}
}

func TestWhereOperatorKinds(t *testing.T) {
	kinds := make(map[string]OperatorKind)
	for _, op := range WhereOperators() {
		if _, dup := kinds[op.Name]; dup {
			t.Errorf("operator %q listed twice", op.Name)
		}
		kinds[op.Name] = op.Kind
	}
	for name, want := range map[string]OperatorKind{
		"==": NumericOperator, "=>": NumericOperator, "<": NumericOperator,
		"eq": StringOperator, "lacks": StringOperator, "nhassuffix": StringOperator,
	} {
		if got, ok := kinds[name]; !ok || got != want {
			t.Errorf("kind of %q = %v (listed %v), want %v", name, got, ok, want)
		}
	}
}

func TestWhereOperatorNegatives(t *testing.T) {
	for _, queryStr := range []string{
		"select foo where foo =~ 1",
		"select foo where foo = 1",
		"select foo where foo starts 1",
		// A numeric operator needs barewords on both sides.
		"select foo where foo == \"1\"",
	} {
		if _, err := NewQuery(queryStr, logging.NopLogger{}); err == nil {
			t.Errorf("NewQuery(%q) succeeded, want an error", queryStr)
		}
	}
}

func TestFunctionNamesParse(t *testing.T) {
	names := funcs.Names()
	if len(names) == 0 {
		t.Fatal("funcs.Names() is empty")
	}
	for _, name := range names {
		queryStr := "select foo set $a = " + name + "(foo)"
		q, err := NewQuery(queryStr, logging.NopLogger{})
		if err != nil {
			t.Errorf("NewQuery(%q): %v", queryStr, err)
			continue
		}
		if q.Set[0].rType != FunctionStack {
			t.Errorf("%q: set value type %v, want a function stack", queryStr,
				q.Set[0].rType)
		}
	}
	if _, err := NewQuery("select foo set $a = sha1(foo)", logging.NopLogger{}); err == nil {
		t.Error("NewQuery with an unknown set function succeeded, want an error")
	}
}

// TestVocabularyReturnsCopies checks that a caller (the completer) changing
// a returned list cannot change what the parser accepts.
func TestVocabularyReturnsCopies(t *testing.T) {
	Keywords()[0] = "changed"
	AggregationNames()[0] = "changed"
	WhereOperators()[0].Name = "changed"
	funcs.Names()[0] = "changed"
	if slices.Contains(Keywords(), "changed") ||
		slices.Contains(AggregationNames(), "changed") ||
		WhereOperators()[0].Name == "changed" ||
		slices.Contains(funcs.Names(), "changed") {
		t.Fatal("a vocabulary accessor returned its table instead of a copy")
	}
	if _, err := NewQuery("select count(foo) where foo == 1", logging.NopLogger{}); err != nil {
		t.Fatalf("NewQuery after changing the returned lists: %v", err)
	}
}

// TestVocabularyMatchesDoc keeps the grammar of doc/querylanguage.md in step
// with the parser's tables.
func TestVocabularyMatchesDoc(t *testing.T) {
	data, err := os.ReadFile("../../doc/querylanguage.md")
	if err != nil {
		t.Fatalf("read query language doc: %v", err)
	}
	doc := string(data)

	operators := make(map[OperatorKind][]string)
	for _, op := range WhereOperators() {
		operators[op.Kind] = append(operators[op.Kind], op.Name)
	}
	checks := []struct {
		rule string
		want []string
		sep  string
	}{
		{"FLOATOPERATOR := One of:", operators[NumericOperator], " "},
		{"STRINGOPERATOR :=", operators[StringOperator], "|"},
		{"AGGREGATION :=", AggregationNames(), "|"},
		{"FUNCTION :=", funcs.Names(), "|"},
	}
	for _, c := range checks {
		got := docRule(t, doc, c.rule, c.sep)
		if !slices.Equal(got, c.want) {
			t.Errorf("doc rule %q lists %v, the parser %v", c.rule, got, c.want)
		}
	}
	if got := docClauseKeywords(doc); !sameSet(got, Keywords()) {
		t.Errorf("doc CLAUSE rule lists keywords %v, the parser %v", got, Keywords())
	}
}

// docRule returns the alternatives of the grammar line starting with rule.
func docRule(t *testing.T, doc, rule, sep string) []string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if rest, ok := strings.CutPrefix(line, rule); ok {
			return strings.Split(strings.TrimSpace(rest), sep)
		}
	}
	t.Fatalf("doc has no grammar line %q", rule)
	return nil
}

// docClauseKeywords returns the first word of every CLAUSE alternative.
func docClauseKeywords(doc string) []string {
	re := regexp.MustCompile(`(?m)^(?:CLAUSE := |\s+\| )([a-z]+) `)
	var keywords []string
	for _, m := range re.FindAllStringSubmatch(doc, -1) {
		keywords = append(keywords, m[1])
	}
	return keywords
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
