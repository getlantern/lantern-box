// Package sync provides a generic, type-safe wrapper around sync.Map
// for concurrent access to maps with arbitrary key and value types.
package sync

import (
	"iter"
	"sync"
)

// TypedMap is a generic, type-safe concurrent map based on sync.Map.
// It supports any comparable key type and any value type.
type TypedMap[K comparable, V any] struct {
	data sync.Map
}

func (m *TypedMap[K, V]) Clear() {
	m.data.Clear()
}

func (m *TypedMap[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	return m.data.CompareAndDelete(key, old)
}

func (m *TypedMap[K, V]) CompareAndSwap(key K, old V, new V) (swapped bool) {
	return m.data.CompareAndSwap(key, old, new)
}

func (m *TypedMap[K, V]) Delete(key K) {
	m.data.Delete(key)
}

func (m *TypedMap[K, V]) Load(key K) (value V, ok bool) {
	v, ok := m.data.Load(key)
	if !ok {
		return zero[V](), false
	}
	return v.(V), true
}

func (m *TypedMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	v, loaded := m.data.LoadAndDelete(key)
	if !loaded {
		return zero[V](), false
	}
	return v.(V), true
}

func (m *TypedMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	v, loaded := m.data.LoadOrStore(key, value)
	if !loaded {
		return value, false
	}
	return v.(V), true
}

func (m *TypedMap[K, V]) Iter() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		m.data.Range(func(key, value any) bool {
			return yield(key.(K), value.(V))
		})
	}
}

func (m *TypedMap[K, V]) Store(key K, value V) {
	m.data.Store(key, value)
}

func (m *TypedMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	prev, loaded := m.data.Swap(key, value)
	if !loaded {
		return zero[V](), false
	}
	return prev.(V), true
}

func zero[V any]() V {
	var zero V
	return zero
}
