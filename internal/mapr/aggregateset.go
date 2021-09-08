package mapr

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
)

// AggregateSet represents aggregated key/value pairs from the
// MAPREDUCE log lines. These could be either string values or float
// values.
type AggregateSet struct {
	Samples int
	FValues map[string]float64
	SValues map[string]string
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
	return fmt.Sprintf("AggregateSet(Samples:%d,FValues:%v,SValues:%v)",
		s.Samples, s.FValues, s.SValues)
}

// Merge one aggregate set into this one.
func (s *AggregateSet) Merge(query *Query, set *AggregateSet) error {
	s.Samples += set.Samples
	//logger.Trace("Merge", set)

	for _, sc := range query.Select {
		storage := sc.FieldStorage
		switch sc.Operation {
		case Count:
			fallthrough
		case Sum:
			fallthrough
		case Avg:
			value := set.FValues[storage]
			s.addFloat(storage, value)
		case Min:
			value := set.FValues[storage]
			s.addFloatMin(storage, value)
		case Max:
			value := set.FValues[storage]
			s.addFloatMax(storage, value)
		case Last:
			value := set.SValues[storage]
			s.setString(storage, value)
		case Len:
			s.setString(storage, set.SValues[storage])
			s.setFloat(storage, set.FValues[storage])
		default:
			return fmt.Errorf("Unknown aggregation method '%v'", sc.Operation)
		}
	}
	return nil
}

// Serialize the aggregate set so it can be sent over the wire.
func (s *AggregateSet) Serialize(ctx context.Context, groupKey string, ch chan<- string) {
	logger.Trace("Serialising mapr.AggregateSet", s)
	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)

	sb.WriteString(groupKey)
	sb.WriteString(protocol.AggregateDelimiter)
	sb.WriteString(fmt.Sprintf("%d", s.Samples))
	sb.WriteString(protocol.AggregateDelimiter)

	for k, v := range s.FValues {
		sb.WriteString(k)
		sb.WriteString(protocol.AggregateKVDelimiter)
		sb.WriteString(fmt.Sprintf("%v", v))
		sb.WriteString(protocol.AggregateDelimiter)
	}

	for k, v := range s.SValues {
		sb.WriteString(k)
		sb.WriteString(protocol.AggregateKVDelimiter)
		sb.WriteString(v)
		sb.WriteString(protocol.AggregateDelimiter)
	}

	select {
	case ch <- sb.String():
	case <-ctx.Done():
	}
}

// Add a float value.
func (s *AggregateSet) addFloat(key string, value float64) {
	if _, ok := s.FValues[key]; !ok {
		s.FValues[key] = value
		return
	}
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

// Set a string.
func (s *AggregateSet) setString(key, value string) {
	s.SValues[key] = value
}

// Set a float.
func (s *AggregateSet) setFloat(key string, value float64) {
	s.FValues[key] = value
}

// Aggregate data to the aggregate set.
func (s *AggregateSet) Aggregate(key string, agg AggregateOperation, value string, clientAggregation bool) (err error) {
	var f float64

	// First check if we can aggregate anything without converting value to float.
	switch agg {
	case Count:
		if clientAggregation {
			f, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return
			}
			s.addFloat(key, f)
			return
		}
		s.addFloat(key, 1)
		return
	case Last:
		s.setString(key, value)
		return
	case Len:
		s.setString(key, value)
		s.setFloat(key, float64(len(value)))
		return
	default:
	}

	// No, we have to convert to float.
	f, err = strconv.ParseFloat(value, 64)
	if err != nil {
		return
	}

	switch agg {
	case Sum:
		fallthrough
	case Avg:
		s.addFloat(key, f)
	case Min:
		s.addFloatMin(key, f)
	case Max:
		s.addFloatMax(key, f)
	default:
		err = fmt.Errorf("Unknown aggregation method '%v'", agg)
	}
	return
}
