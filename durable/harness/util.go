package harness

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// toValue converts a Go value to the chord representation documents hold:
// nil, bool, float64, string, []any and map[string]any.
func toValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// fromValue decodes a document value into out; a nil value leaves out alone.
func fromValue(value map[string]any, out any) error {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// sortedKeys returns the keys of m in ascending order, for deterministic
// writes.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ordered is a JS Map: setting an existing key keeps its position, a new key
// appends, and deleting removes it.
type ordered[T any] struct {
	keys  []string
	byKey map[string]T
}

func newOrdered[T any]() *ordered[T] { return &ordered[T]{byKey: map[string]T{}} }

func (o *ordered[T]) get(key string) (T, bool) {
	value, ok := o.byKey[key]
	return value, ok
}

func (o *ordered[T]) set(key string, value T) {
	if _, ok := o.byKey[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.byKey[key] = value
}

func (o *ordered[T]) delete(key string) {
	if _, ok := o.byKey[key]; !ok {
		return
	}
	delete(o.byKey, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *ordered[T]) values() []T {
	out := make([]T, len(o.keys))
	for i, key := range o.keys {
		out[i] = o.byKey[key]
	}
	return out
}

// panicError turns a recovered panic into an error.
func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}

// scanAll returns every item of a paginated scan, in page order.
func scanAll[T any](scan func(cursor durable.Cursor) (durable.Page[T], error)) ([]T, error) {
	var items []T
	var cursor durable.Cursor
	for {
		page, err := scan(cursor)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Next == nil {
			return items, nil
		}
		cursor = page.Next
	}
}
