// Package nodefacts turns stored node diagnostics into a tri-state fact set and
// evaluates boolean conditions over it. It deliberately knows nothing about
// tags: callers (today the tagging service, tomorrow any screening-rule engine)
// supply the conditions and interpret the result.
package nodefacts

import (
	"strconv"
	"strings"
	"time"
)

// Fact is the tri-state carrier for one node attribute. Known=false means "no
// measurement", which is not the same as a zero value: an unknown fact fails
// every operator except is_unknown/is_known, negative ones (ne, not_in,
// not_contains) included.
type Fact[T any] struct {
	Value     T
	Known     bool
	CheckedAt time.Time
}

// Known wraps a value whose freshness is irrelevant (configuration, not
// measurement), so max_age_seconds can never expire it.
func Known[T any](value T) Fact[T] {
	return Fact[T]{Value: value, Known: true}
}

// KnownAt wraps a measured value together with the time it was measured.
func KnownAt[T any](value T, checkedAt time.Time) Fact[T] {
	return Fact[T]{Value: value, Known: true, CheckedAt: checkedAt}
}

// Unknown is the explicit "not measured" fact.
func Unknown[T any]() Fact[T] {
	return Fact[T]{}
}

// Kind is the value domain of a field, and decides which operators apply.
type Kind string

const (
	KindString Kind = "string"
	KindEnum   Kind = "enum"
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindSet    Kind = "set"
)

// Value is the type-erased form of a Fact. The evaluator works on these because
// a fact set is a heterogeneous map keyed by field name.
type Value struct {
	Kind      Kind
	Known     bool
	CheckedAt time.Time
	Str       string
	Num       int64
	Bool      bool
	Set       []string
}

// StringValue, EnumValue, IntValue, BoolValue and SetValue erase a typed fact.
func StringValue(fact Fact[string]) Value {
	return Value{Kind: KindString, Known: fact.Known, CheckedAt: fact.CheckedAt, Str: fact.Value}
}

func EnumValue(fact Fact[string]) Value {
	return Value{Kind: KindEnum, Known: fact.Known, CheckedAt: fact.CheckedAt, Str: fact.Value}
}

func IntValue(fact Fact[int64]) Value {
	return Value{Kind: KindInt, Known: fact.Known, CheckedAt: fact.CheckedAt, Num: fact.Value}
}

func BoolValue(fact Fact[bool]) Value {
	return Value{Kind: KindBool, Known: fact.Known, CheckedAt: fact.CheckedAt, Bool: fact.Value}
}

func SetValue(fact Fact[[]string]) Value {
	return Value{Kind: KindSet, Known: fact.Known, CheckedAt: fact.CheckedAt, Set: fact.Value}
}

// Aged returns the value with Known cleared when the measurement is older than
// maxAge. A zero CheckedAt is treated as timeless: facts that carry no timestamp
// (node configuration, manual tags) are never expired by a rule.
func (v Value) Aged(maxAge time.Duration, now time.Time) Value {
	if !v.Known || maxAge <= 0 || v.CheckedAt.IsZero() {
		return v
	}
	if now.Sub(v.CheckedAt) > maxAge {
		v.Known = false
	}
	return v
}

// Display renders a value for the preview response. Unknown facts render as "-"
// so the operator can see which facts were missing rather than guessing.
func (v Value) Display() string {
	if !v.Known {
		return "-"
	}
	switch v.Kind {
	case KindInt:
		return strconv.FormatInt(v.Num, 10)
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindSet:
		if len(v.Set) == 0 {
			return "[]"
		}
		return strings.Join(v.Set, ", ")
	default:
		return v.Str
	}
}

// NodeFacts is one node's fact set. Name and Region are carried separately
// because previews label rows with them.
type NodeFacts struct {
	NodeID int64
	Name   string
	Region string
	Values map[string]Value
}

// NewNodeFacts returns an empty fact set for a node.
func NewNodeFacts(nodeID int64, name, region string) NodeFacts {
	return NodeFacts{NodeID: nodeID, Name: name, Region: region, Values: map[string]Value{}}
}

// Value returns the fact for a field. An unregistered or unloaded field yields
// the zero Value, which is unknown — a rule referencing it simply never matches.
func (f NodeFacts) Value(field string) Value {
	if f.Values == nil {
		return Value{}
	}
	return f.Values[field]
}

// Set stores a fact, ignoring empty field names.
func (f NodeFacts) Set(field string, value Value) {
	if f.Values == nil || field == "" {
		return
	}
	f.Values[field] = value
}
