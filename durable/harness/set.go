package harness

import "container/list"

// set is an insertion-ordered set, pi's Set: iteration follows insertion, and
// re-adding a member keeps its place. Add, delete and has are O(1).
type set[T comparable] struct {
	order *list.List
	index map[T]*list.Element
}

func newSet[T comparable]() *set[T] {
	return &set[T]{order: list.New(), index: map[T]*list.Element{}}
}

func (s *set[T]) add(v T) {
	if _, ok := s.index[v]; !ok {
		s.index[v] = s.order.PushBack(v)
	}
}

func (s *set[T]) delete(v T) {
	if e, ok := s.index[v]; ok {
		s.order.Remove(e)
		delete(s.index, v)
	}
}

func (s *set[T]) has(v T) bool {
	_, ok := s.index[v]
	return ok
}

func (s *set[T]) len() int { return len(s.index) }

func (s *set[T]) clear() {
	s.order.Init()
	s.index = map[T]*list.Element{}
}

// values returns the members in insertion order, as a copy safe to iterate
// while the set changes.
func (s *set[T]) values() []T {
	out := make([]T, 0, len(s.index))
	for e := s.order.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(T))
	}
	return out
}

// addTo adds value to the set under key, creating it.
func addTo[K, V comparable](m map[K]*set[V], key K, value V) {
	s := m[key]
	if s == nil {
		s = newSet[V]()
		m[key] = s
	}
	s.add(value)
}

// deleteFrom removes value from the set under key, dropping an emptied set.
func deleteFrom[K, V comparable](m map[K]*set[V], key K, value V) {
	s := m[key]
	if s == nil {
		return
	}
	s.delete(value)
	if s.len() == 0 {
		delete(m, key)
	}
}
