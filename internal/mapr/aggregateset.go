package mapr

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
)

// avgCountSuffix turns an avg() storage key into the wire key of the number
// of values its sum holds (see AvgCountKey). It starts with a double quote,
// which no select storage key can contain (the query tokenizer splits on it),
// so the count key never collides with the storage key of a selected field,
// also not on an older client that receives it.
const avgCountSuffix = "\"count"

// AggregateSet represents aggregated key/value pairs from the
// MAPREDUCE log lines. These could be either string values or float
// values.
type AggregateSet struct {
	// Samples counts the lines of the group in which at least one selected
	// field could be aggregated. avg() does not divide by it (see
	// ValueCounts); it is still sent because an older client divides avg() by
	// it, and it stands in for the value count an older server does not send.
	Samples int
	FValues map[string]float64
	SValues map[string]string
	// ValueCounts holds, per avg() storage key, the number of values its sum
	// in FValues holds, so a line lacking the averaged field, or whose value
	// is not a number, does not lower the average. It is keyed like FValues
	// (no per-line key to build) and is nil until the first avg() value.
	ValueCounts map[string]int
}

// NewAggregateSet creates a new empty aggregate set.
func NewAggregateSet() *AggregateSet {
	return &AggregateSet{
		FValues: make(map[string]float64),
		SValues: make(map[string]string),
	}
}

// String representation of aggregate set.
func (s *AggregateSet) String() string {
	return fmt.Sprintf("AggregateSet(Samples:%d,FValues:%v,SValues:%v,ValueCounts:%v)",
		s.Samples, s.FValues, s.SValues, s.ValueCounts)
}

// AvgCountKey returns the key under which a serialized set carries the number
// of values the avg() sum of storage holds (ValueCounts[storage]).
func AvgCountKey(storage string) string {
	return storage + avgCountSuffix
}

// Merge one aggregate set into this one. set is treated as the newer of the
// two: where both carry a last() or len() value, set's value wins.
//
// A field is merged only when set carries its storage key. A set has a key
// only when a line of its group had the field (sparse fields, where clauses
// not requiring it, a later interval, or another server's lines), so an
// absent key means "no value", not 0 or "": merging it as one would turn
// min() of positive or max() of negative values into 0 and blank last() and
// len(). Skipping it also keeps additive operations from creating keys no
// line ever had: they render as 0 either way, but percentile() ranks only the
// groups that have the key (GroupSet.makeResultStats).
//
// On error (an unknown aggregation, which the query parser rejects) s is left
// unchanged: every operation is checked before anything is merged, so a caller
// never ends up with a partially merged set. Both passes look the operation up
// in fieldMergerFor, the single place that knows how each operation merges.
func (s *AggregateSet) Merge(query *Query, set *AggregateSet) error {
	for _, sc := range query.Select {
		if fieldMergerFor(sc.Operation) == nil {
			return fmt.Errorf("unknown aggregation method '%v'", sc.Operation)
		}
	}
	s.Samples += set.Samples
	for _, sc := range query.Select {
		fieldMergerFor(sc.Operation)(s, sc.FieldStorage, set)
	}
	return nil
}

// Serialize the aggregate set so it can be sent over the wire. The value
// count of each avg() field is sent as one more key/value pair under
// AvgCountKey; an older client only reads the keys of its selected fields and
// ignores it.
//
// Each count directly follows its own sum, before any last()/len() string.
// Should a message reach the client cut short (e.g. a frame split inside a
// last() string), a count is then lost only together with its sum or right
// after it, never because of a string written between the two, so the client
// falls back to the message's Samples for at most that one avg() field.
// Returns true when the serialized message was successfully sent, and false
// when the context was cancelled before the send completed. Callers that own
// the source state (e.g. Aggregate) must re-merge unsent sets so data is not
// silently lost.
func (s *AggregateSet) Serialize(ctx context.Context, groupKey string, ch chan<- string,
	logger logging.Logger) bool {

	logger = logging.OrNop(logger)
	if logger.TraceEnabled() {
		logger.Trace("Serialising mapr.AggregateSet", s)
	}
	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)

	sb.WriteString(sanitizeSerialized(groupKey))
	sb.WriteString(protocol.AggregateDelimiter)
	sb.WriteString(strconv.Itoa(s.Samples))
	sb.WriteString(protocol.AggregateDelimiter)

	for k, v := range s.FValues {
		writeSerializedField(sb, k, "", strconv.FormatFloat(v, 'f', -1, 64))
		// Only avg() sums have a count (AggregateAvg adds both); the lookup
		// in the usually nil ValueCounts map costs no allocation.
		if n, ok := s.ValueCounts[k]; ok {
			writeSerializedField(sb, k, avgCountSuffix, strconv.Itoa(n))
		}
	}
	for k, v := range s.SValues {
		writeSerializedField(sb, k, "", sanitizeSerialized(v))
	}

	select {
	case ch <- sb.String():
		return true
	case <-ctx.Done():
		return false
	}
}

// serializedDelimiterReplacement stands in for protocol.AggregateDelimiter
// within a group key or string value of a serialized set.
const serializedDelimiterReplacement = "||"

// sanitizeSerialized returns value without protocol.AggregateDelimiter. The
// wire format has no escaping, so log content containing the delimiter would
// otherwise end the value early and have its rest parsed as further fields:
// a group key with it lost the whole group ("unable to parse sample count"),
// and a last()/len() value with it could overwrite any numeric column of its
// row. The replacement is one-way (the value shows "||" for the delimiter),
// which needs no capability and works with every client version. Values
// without the delimiter, i.e. practically all, are returned as they are.
func sanitizeSerialized(value string) string {
	if !strings.Contains(value, protocol.AggregateDelimiter) {
		return value
	}
	return strings.ReplaceAll(value, protocol.AggregateDelimiter, serializedDelimiterReplacement)
}

// writeSerializedField appends one key/value pair of a serialized set. The
// key is written as storage followed by suffix, so that the avg() count key
// (AvgCountKey) needs no string of its own.
func writeSerializedField(sb *strings.Builder, storage, suffix, value string) {
	sb.WriteString(storage)
	sb.WriteString(suffix)
	sb.WriteString(protocol.AggregateKVDelimiter)
	sb.WriteString(value)
	sb.WriteString(protocol.AggregateDelimiter)
}

// fieldMerger merges the value of one storage key of set into s, if set has
// one (see Merge).
type fieldMerger func(s *AggregateSet, storage string, set *AggregateSet)

// fieldMergerFor returns how op merges, or nil when Merge does not know op.
// It is the only list of mergeable operations: Merge uses it both to reject an
// unknown operation and to merge, so an operation cannot be accepted by the
// check but then silently skipped by the merge. A new operation must be added
// here (TestFieldMergerForCoversAggregate checks it against Aggregate).
func fieldMergerFor(op AggregateOperation) fieldMerger {
	switch op {
	case Count, Sum, Percentage, Percentile:
		return mergeAdditive
	case Avg:
		return mergeAvg
	case Min:
		return mergeMin
	case Max:
		return mergeMax
	case Last:
		return mergeLast
	case Len:
		return mergeLen
	default:
		return nil
	}
}

func mergeAdditive(s *AggregateSet, storage string, set *AggregateSet) {
	if value, ok := set.FValues[storage]; ok {
		s.addFloat(storage, value)
	}
}

// mergeAvg merges an avg() sum and the number of values it holds. A set that
// has the sum has its count too (AggregateAvg adds both), so both are merged
// or neither is.
func mergeAvg(s *AggregateSet, storage string, set *AggregateSet) {
	mergeAdditive(s, storage, set)
	if count, ok := set.ValueCounts[storage]; ok {
		s.addValueCount(storage, count)
	}
}

func mergeMin(s *AggregateSet, storage string, set *AggregateSet) {
	if value, ok := set.FValues[storage]; ok {
		s.addFloatMin(storage, value)
	}
}

func mergeMax(s *AggregateSet, storage string, set *AggregateSet) {
	if value, ok := set.FValues[storage]; ok {
		s.addFloatMax(storage, value)
	}
}

func mergeLast(s *AggregateSet, storage string, set *AggregateSet) {
	if value, ok := set.SValues[storage]; ok {
		s.setString(storage, value)
	}
}

// mergeLen merges a len() value. Aggregate stores it as the string and its
// length under the same key, so the string's presence stands for both.
func mergeLen(s *AggregateSet, storage string, set *AggregateSet) {
	if value, ok := set.SValues[storage]; ok {
		s.setString(storage, value)
		s.setFloat(storage, set.FValues[storage])
	}
}

// Add a float value.
func (s *AggregateSet) addFloat(key string, value float64) {
	// A missing key reads as 0, so one map operation does.
	s.FValues[key] += value
}

// Add a float minimum value.
func (s *AggregateSet) addFloatMin(key string, value float64) {
	f, ok := s.FValues[key]
	if !ok {
		s.FValues[key] = value
		return
	}
	if f > value {
		s.FValues[key] = value
	}
}

// Add a float maximum value.
func (s *AggregateSet) addFloatMax(key string, value float64) {
	f, ok := s.FValues[key]
	if !ok {
		s.FValues[key] = value
		return
	}
	if f < value {
		s.FValues[key] = value
	}
}

// addValueCount adds count to the number of values the avg() sum of storage
// holds, allocating ValueCounts on the first avg() value.
func (s *AggregateSet) addValueCount(storage string, count int) {
	if s.ValueCounts == nil {
		s.ValueCounts = make(map[string]int)
	}
	s.ValueCounts[storage] += count
}

// Set a string.
func (s *AggregateSet) setString(key, value string) {
	s.SValues[key] = value
}

// setStringCopy stores a copy of value, which the caller only lends. A value
// equal to the stored one is kept as it is: a plain select field is a last(),
// and for the usual "select host,count(...) group by host" the value repeats
// on every line of the group, so this saves an allocation per line under the
// aggregate's group lock.
func (s *AggregateSet) setStringCopy(key, value string) {
	if old, ok := s.SValues[key]; ok && old == value {
		return
	}
	s.setString(key, strings.Clone(value))
}

// parseNumber parses value as a finite number. strconv.ParseFloat also
// accepts "NaN" and "Inf": one such log value would turn a sum or avg() into
// NaN for good and make min()/max() depend on the order of the lines.
func parseNumber(value string) (float64, error) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("'%s' is not a finite number", value)
	}
	return f, nil
}

// Set a float.
func (s *AggregateSet) setFloat(key string, value float64) {
	s.FValues[key] = value
}

// AggregateAvg adds value, a number, to the avg() sum of storage and count to
// the number of values that sum holds. A dserver aggregating a line passes a
// count of 1; a client aggregating a server's sum passes the count the server
// sent with it. A value that is not a number adds neither, so it does not
// lower the average.
func (s *AggregateSet) AggregateAvg(storage, value string, count int) error {
	f, err := parseNumber(value)
	if err != nil {
		return err
	}
	s.addFloat(storage, f)
	s.addValueCount(storage, count)
	return nil
}

// Aggregate data to the aggregate set. The value is only borrowed for the
// duration of the call: every string the set keeps is stored as a copy.
//
// With clientAggregation the value is one a server already aggregated. An
// avg() sum then needs the number of values it holds, which only the caller
// knows, so it must be added with AggregateAvg instead.
func (s *AggregateSet) Aggregate(key string, agg AggregateOperation, value string, clientAggregation bool) error {
	switch agg {
	case Count:
		return s.aggregateCount(key, value, clientAggregation)
	case Last:
		// value may borrow memory that the caller reuses for the next line
		// (see aggregate.parseLine), so anything retained must be a copy.
		s.setStringCopy(key, value)
		return nil
	case Len:
		s.setStringCopy(key, value)
		s.setFloat(key, float64(len(value)))
		return nil
	case Avg:
		if clientAggregation {
			return fmt.Errorf("aggregated avg() value of '%s' needs its value count", key)
		}
		return s.AggregateAvg(key, value, 1)
	case Sum, Percentage, Percentile, Min, Max:
		return s.aggregateNumber(key, agg, value)
	default:
		return fmt.Errorf("unknown aggregation method '%v'", agg)
	}
}

// aggregateCount counts one line, or with clientAggregation adds the count a
// server sent.
func (s *AggregateSet) aggregateCount(key, value string, clientAggregation bool) error {
	if !clientAggregation {
		s.addFloat(key, 1)
		return nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return err
	}
	s.addFloat(key, f)
	return nil
}

// aggregateNumber aggregates value, which must be a number, with one of the
// numeric operations sum, percentage, percentile, min and max.
func (s *AggregateSet) aggregateNumber(key string, agg AggregateOperation, value string) error {
	f, err := parseNumber(value)
	if err != nil {
		return err
	}
	switch agg {
	case Min:
		s.addFloatMin(key, f)
	case Max:
		s.addFloatMax(key, f)
	default:
		// Sum, Percentage and Percentile all add up the values.
		s.addFloat(key, f)
	}
	return nil
}

// avg returns the average of the values aggregated into the avg() sum of
// storage, or 0 when there are none. A set without a value count for it,
// which AggregateAvg never makes, falls back to Samples, as avg() did before
// value counts existed.
func (s *AggregateSet) avg(storage string) float64 {
	count, ok := s.ValueCounts[storage]
	if !ok {
		count = s.Samples
	}
	// Guard against 0/0, which would render as "NaN" (e.g. an empty set).
	if count == 0 {
		return 0
	}
	return s.FValues[storage] / float64(count)
}
