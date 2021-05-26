package tracker

import (
	"fmt"

	"github.com/pelletier/go-toml/v2/internal/ast"
	"github.com/segmentio/fasthash/fnv1a"
)

type keyKind uint8

const (
	invalidKind keyKind = iota
	valueKind
	tableKind
	arrayTableKind
)

func (k keyKind) String() string {
	switch k {
	case invalidKind:
		return "invalid"
	case valueKind:
		return "value"
	case tableKind:
		return "table"
	case arrayTableKind:
		return "array table"
	}
	panic("missing keyKind string mapping")
}

// SeenTracker tracks which keys have been seen with which TOML type to flag duplicates
// and mismatches according to the spec.
type SeenTracker struct {
	root    uint64
	current uint64
	entries []entry
}

type entry struct {
	hash     uint64
	parent   uint64
	kind     keyKind
	explicit bool
}

// CheckExpression takes a top-level node and checks that it does not contain keys
// that have been seen in previous calls, and validates that types are consistent.
func (s *SeenTracker) CheckExpression(node ast.Node) error {
	if s.entries == nil {
		s.entries = make([]entry, 0, 8)
		s.root = fnv1a.Init64
		s.current = fnv1a.Init64
	}

	switch node.Kind {
	case ast.KeyValue:
		return s.checkKeyValue(node)
	case ast.Table:
		return s.checkTable(node)
	case ast.ArrayTable:
		return s.checkArrayTable(node)
	default:
		panic(fmt.Errorf("this should not be a top level node type: %s", node.Kind))
	}

}
func (s *SeenTracker) checkTable(node ast.Node) error {
	s.current = s.root

	it := node.Key()
	h := s.current
	idx := 0

	// handle the first parts of the key, excluding the last one
	for it.Next() {
		if !it.Node().Next().Valid() {
			break
		}

		parent := h
		h = fnv1a.AddUint64(h, 0)
		h = fnv1a.AddBytes64(h, it.Node().Data)

		var found bool
		idx, found = s.findHash(h)

		if !found {
			idx = len(s.entries)
			s.create(h, parent, tableKind, false)
		}
	}

	// handle the last part of the key
	parent := h
	h = fnv1a.AddUint64(h, 0)
	h = fnv1a.AddBytes64(h, it.Node().Data)

	var found bool
	idx, found = s.findHash(h)

	if found {
		e := s.entries[idx]
		k := e.kind
		if k != tableKind {
			return fmt.Errorf("toml: key %s should be a table, not a %s", string(it.Node().Data), k)
		}
		if e.explicit {
			return fmt.Errorf("toml: table %s already exists", string(it.Node().Data))
		}
		s.entries[idx].explicit = true
	} else {
		s.create(h, parent, tableKind, true)
	}

	s.current = h

	return nil
}

func (s *SeenTracker) checkArrayTable(node ast.Node) error {
	s.current = s.root

	it := node.Key()
	h := s.current
	idx := 0

	// handle the first parts of the key, excluding the last one
	for it.Next() {
		if !it.Node().Next().Valid() {
			break
		}

		parent := h
		h = fnv1a.AddUint64(h, 0)
		h = fnv1a.AddBytes64(h, it.Node().Data)

		var found bool
		idx, found = s.findHash(h)

		if !found {
			idx = len(s.entries)
			s.create(h, parent, tableKind, false)
		}
	}

	// handle the last part of the key
	parent := h
	h = fnv1a.AddUint64(h, 0)
	h = fnv1a.AddBytes64(h, it.Node().Data)

	var found bool
	idx, found = s.findHash(h)

	if found {
		e := s.entries[idx]
		k := e.kind
		if k != arrayTableKind {
			return fmt.Errorf("toml: key %s already exists as a %s,  but should be an array table", string(it.Node().Data), k)
		}
		s.clear(h, idx)
	} else {
		s.create(h, parent, arrayTableKind, true)
	}
	s.current = h

	return nil
}

// Remove entries that have p as parent, and all their descendents.
// Starts the search after idx.
// Preserves order.
func (s *SeenTracker) clear(p uint64, idx int) {
	x := clearEntries(p, s.entries[idx+1:])
	s.entries = s.entries[:idx+1+len(x)]
}

func clearEntries(p uint64, s []entry) []entry {
	for i := 0; i < len(s); {
		if s[i].parent == p {
			h := s[i].hash
			copy(s[i:], s[i+1:])
			s = s[:len(s)-1]
			rest := clearEntries(h, s[i:])
			s = s[:i+len(rest)]
		} else {
			i++
		}
	}
	return s
}

func (s *SeenTracker) findHash(h uint64) (int, bool) {
	for i := range s.entries {
		if s.entries[i].hash == h {
			return i, true
		}
	}
	return 0, false
}

func (s *SeenTracker) create(h uint64, parent uint64, k keyKind, explicit bool) {
	s.entries = append(s.entries, entry{
		hash:     h,
		parent:   parent,
		kind:     k,
		explicit: explicit,
	})
}

func (s *SeenTracker) checkKeyValue(node ast.Node) error {
	it := node.Key()

	h := s.current
	idx := 0

	for it.Next() {
		parent := h
		h = fnv1a.AddUint64(h, 0)
		h = fnv1a.AddBytes64(h, it.Node().Data)

		var found bool
		idx, found = s.findHash(h)
		if found {
			if s.entries[idx].kind != tableKind {
				return fmt.Errorf("toml: expected %s to be a table, not a %s", string(it.Node().Data), s.entries[idx].kind)
			}
		} else {
			idx = len(s.entries)
			s.create(h, parent, tableKind, false)
		}
	}

	k := valueKind
	if node.Value().Kind == ast.InlineTable {
		k = tableKind
	}
	s.entries[idx].kind = k

	return nil
}
