package toml

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"reflect"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2/internal/ast"
	"github.com/pelletier/go-toml/v2/internal/tracker"
)

// Unmarshal deserializes a TOML document into a Go value.
//
// It is a shortcut for Decoder.Decode() with the default options.
func Unmarshal(data []byte, v interface{}) error {
	p := parser{}
	p.Reset(data)
	d := decoder{p: &p}

	return d.FromParser(v)
}

// Decoder reads and decode a TOML document from an input stream.
type Decoder struct {
	// input
	r io.Reader

	// global settings
	strict bool
}

// NewDecoder creates a new Decoder that will read from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// SetStrict toggles decoding in stict mode.
//
// When the decoder is in strict mode, it will record fields from the document
// that could not be set on the target value. In that case, the decoder returns
// a StrictMissingError that can be used to retrieve the individual errors as
// well as generate a human readable description of the missing fields.
func (d *Decoder) SetStrict(strict bool) {
	d.strict = strict
}

// Decode the whole content of r into v.
//
// By default, values in the document that don't exist in the target Go value
// are ignored. See Decoder.SetStrict() to change this behavior.
//
// When a TOML local date, time, or date-time is decoded into a time.Time, its
// value is represented in time.Local timezone. Otherwise the approriate Local*
// structure is used.
//
// Empty tables decoded in an interface{} create an empty initialized
// map[string]interface{}.
//
// Types implementing the encoding.TextUnmarshaler interface are decoded from a
// TOML string.
//
// When decoding a number, go-toml will return an error if the number is out of
// bounds for the target type (which includes negative numbers when decoding
// into an unsigned int).
//
// Type mapping
//
// List of supported TOML types and their associated accepted Go types:
//
//   String           -> string
//   Integer          -> uint*, int*, depending on size
//   Float            -> float*, depending on size
//   Boolean          -> bool
//   Offset Date-Time -> time.Time
//   Local Date-time  -> LocalDateTime, time.Time
//   Local Date       -> LocalDate, time.Time
//   Local Time       -> LocalTime, time.Time
//   Array            -> slice and array, depending on elements types
//   Table            -> map and struct
//   Inline Table     -> same as Table
//   Array of Tables  -> same as Array and Table
func (d *Decoder) Decode(v interface{}) error {
	b, err := ioutil.ReadAll(d.r)
	if err != nil {
		return fmt.Errorf("toml: %w", err)
	}

	p := parser{}
	p.Reset(b)
	dec := decoder{
		p: &p,
		strict: strict{
			Enabled: d.strict,
		},
	}

	return dec.FromParser(v)
}

type decoder struct {
	// Which parser instance in use for this decoding session.
	// TODO: Think about removing later.
	p *parser

	// Flag indicating that the current expression is stashed.
	// If set to true, calling nextExpr will not actually pull a new expression
	// but turn off the flag instead.
	stashedExpr bool

	// Skip expressions until a table is found. This is set to true when a
	// table could not be create (missing field in map), so all KV expressions
	// need to be skipped.
	skipUntilTable bool

	// Tracks position in Go arrays.
	// This is used when decoding [[array tables]] into Go arrays. Given array
	// tables are separate TOML expression, we need to keep track of where we
	// are at in the Go array, as we can't just introspect its size.
	arrayIndexes map[reflect.Value]int

	// Tracks keys that have been seen, with which type.
	seen tracker.SeenTracker

	// Strict mode
	strict strict
}

func (d *decoder) expr() ast.Node {
	return d.p.Expression()
}

func (d *decoder) nextExpr() bool {
	if d.stashedExpr {
		d.stashedExpr = false
		return true
	}
	return d.p.NextExpression()
}

func (d *decoder) stashExpr() {
	d.stashedExpr = true
}

func (d *decoder) arrayIndex(shouldAppend bool, v reflect.Value) int {
	if d.arrayIndexes == nil {
		d.arrayIndexes = make(map[reflect.Value]int, 1)
	}

	idx, ok := d.arrayIndexes[v]

	if !ok {
		d.arrayIndexes[v] = 0
	} else if shouldAppend {
		idx++
		d.arrayIndexes[v] = idx
	}

	return idx
}

func (d *decoder) FromParser(v interface{}) error {
	r := reflect.ValueOf(v)
	if r.Kind() != reflect.Ptr {
		return fmt.Errorf("toml: decoding can only be performed into a pointer, not %s", r.Kind())
	}

	if r.IsNil() {
		return fmt.Errorf("toml: decoding pointer target cannot be nil")
	}

	err := d.fromParser(r.Elem())
	if err == nil {
		return d.strict.Error(d.p.data)
	}

	var e *decodeError
	if errors.As(err, &e) {
		return wrapDecodeError(d.p.data, e)
	}

	return err
}

func (d *decoder) fromParser(root reflect.Value) error {
	for d.nextExpr() {
		err := d.handleRootExpression(d.expr(), root)
		if err != nil {
			return err
		}
	}

	return d.p.Error()
}

/*
Rules for the unmarshal code:

- The stack is used to keep track of which values need to be set where.
- handle* functions <=> switch on a given ast.Kind.
- unmarshalX* functions need to unmarshal a node of kind X.
- An "object" is either a struct or a map.
*/

func (d *decoder) handleRootExpression(expr ast.Node, v reflect.Value) error {
	if !expr.Valid() {
		panic("should only be called with a valid expression")
	}

	switch expr.Kind {
	case ast.KeyValue:
		return d.handleKeyValue(expr.Key(), expr.Value(), v)
	case ast.Table:
		return d.handleTable(expr.Key(), v)
	case ast.ArrayTable:
		return d.handleArrayTable(expr.Key(), v)
	default:
		panic(fmt.Errorf("parser should not permit expression of kind %s at document root", expr.Kind))
	}

	return nil
}

func (d *decoder) handleArrayTable(key ast.Iterator, v reflect.Value) error {
	if key.Next() {
		return d.handleArrayTablePart(key, v)
	}

	var elem reflect.Value

	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			v.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
		}
		elem = reflect.New(v.Type().Elem()).Elem()
	case reflect.Interface:
		elem = v.Elem()
		if !elem.IsValid() {
			elem = reflect.New(sliceInterfaceType).Elem()
			elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
		}
		err := d.handleArrayTable(key, elem)
		if err != nil {
			return err
		}
		v.Set(elem)
		return nil
	default:
		return fmt.Errorf("toml: array table needs to point to a slice or array, not a %s", v.Kind())
	}

	// Done scoping the key.
	// Now handle all the key-value expressions in this table.
	err := d.handleKeyValues(elem)
	if err != nil {
		return err
	}

	v.Set(reflect.Append(v, elem))
	return nil
}

// HandleArrayTablePart navigates the Go structure v using the key v. It is
// only used for the prefix (non-last) parts of an array-table. When
// encountering a collection, it should go to the last element.
//
// TODO: this function is basically a copy-paste from handleArrayPart.
//   Find a way to refactor.
func (d *decoder) handleArrayTablePart(key ast.Iterator, v reflect.Value) error {
	object := v
	shouldSet := false

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Slice:
		elem := v.Index(v.Len() - 1)
		return d.handleArrayTablePart(key, elem)
	case reflect.Map:
		// Create the key for the map element. For now assume it's a string.
		mk := reflect.ValueOf(string(key.Node().Data))

		// If the map does not exist, create it.
		if v.IsNil() {
			object = reflect.MakeMap(v.Type())
			shouldSet = true
		}

		mv := object.MapIndex(mk)
		if !mv.IsValid() {
			mv = reflect.New(object.Type().Elem()).Elem()
		} else if mv.Kind() == reflect.Interface {
			elem := mv.Elem()
			if elem.IsValid() {
				mv = elem
			} else {
				last := key.Node().Next().Valid()

				if last {
					mv = reflect.New(sliceInterfaceType).Elem()
					mv.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
				} else {
					mv = reflect.MakeMap(mapStringInterfaceType)
				}
			}
		}

		err := d.handleArrayTable(key, mv)
		if err != nil {
			return err
		}

		object.SetMapIndex(mk, mv)
	case reflect.Struct:
		f, found, err := structField(v, string(key.Node().Data))
		if err != nil || !found {
			return err
		}

		return d.handleArrayTable(key, f)
	case reflect.Interface:
		if v.Elem().IsValid() {
			object = v.Elem()
		} else {
			object = reflect.MakeMap(mapStringInterfaceType)
			shouldSet = true
		}

		err := d.handleArrayTablePart(key, object)
		if err != nil {
			return err
		}
	default:
		panic(fmt.Errorf("unhandled: %s", v.Kind()))
	}

	if shouldSet {
		v.Set(object)
	}

	return nil
}

// HandleTable returns a reference when it has checked the next expression but
// cannot handle it.
func (d *decoder) handleTable(key ast.Iterator, v reflect.Value) error {
	if key.Next() {
		// Still scoping the key
		return d.handleTablePart(key, v)
	}
	// Done scoping the key.
	// Now handle all the key-value expressions in this table.
	return d.handleKeyValues(v)
}

// Handle root expressions until the end of the document or the next
// non-key-value.
func (d *decoder) handleKeyValues(v reflect.Value) error {
	for d.nextExpr() {
		expr := d.expr()
		if expr.Kind != ast.KeyValue {
			// Stash the expression so that fromParser can just loop and use
			// the right handler.
			// We could just recurse ourselves here, but at least this gives a
			// chance to pop the stack a bit.
			d.stashExpr()
			return nil
		}

		err := d.handleKeyValue(expr.Key(), expr.Value(), v)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) handleTablePart(key ast.Iterator, v reflect.Value) error {
	object := v
	shouldSet := false

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Map:
		// Create the key for the map element. For now assume it's a string.
		mk := reflect.ValueOf(string(key.Node().Data))

		// If the map does not exist, create it.
		if v.IsNil() {
			object = reflect.MakeMap(v.Type())
			shouldSet = true
		}

		mv := object.MapIndex(mk)
		if !mv.IsValid() {
			t := object.Type().Elem()
			if t.Kind() == reflect.Interface {
				mv = reflect.MakeMap(mapStringInterfaceType)
			} else {
				mv = reflect.New(t).Elem()
			}
		} else if mv.Kind() == reflect.Interface {
			elem := mv.Elem()
			if elem.IsValid() {
				mv = elem
			} else {
				mv = reflect.MakeMap(mapStringInterfaceType)
			}
		}

		err := d.handleTable(key, mv)
		if err != nil {
			return err
		}

		object.SetMapIndex(mk, mv)
	case reflect.Struct:
		f, found, err := structField(v, string(key.Node().Data))
		if err != nil || !found {
			return err
		}

		return d.handleTable(key, f)
	case reflect.Interface:
		if v.Elem().IsValid() {
			object = v.Elem()
		} else {
			object = reflect.MakeMap(mapStringInterfaceType)
			shouldSet = true
		}

		err := d.handleTablePart(key, object)
		if err != nil {
			return err
		}
	default:
		panic(fmt.Errorf("unhandled: %s", v.Kind()))
	}

	if shouldSet {
		v.Set(object)
	}

	return nil
}

func (d *decoder) handleValue(value ast.Node, v reflect.Value) error {
	assertSettable(v)

	for v.Kind() == reflect.Ptr {
		v = initAndDereferencePointer(v)
	}

	switch value.Kind {
	case ast.String:
		return d.unmarshalString(value, v)
	case ast.Integer:
		return d.unmarshalInteger(value, v)
	case ast.Float:
		return d.unmarshalFloat(value, v)
	case ast.Bool:
		return d.unmarshalBool(value, v)
	case ast.DateTime:
		return d.unmarshalDateTime(value, v)
	case ast.InlineTable:
		return d.unmarshalInlineTable(value, v)
	case ast.Array:
		return d.unmarshalArray(value, v)
	default:
		panic(fmt.Errorf("handleValue not implemented for %s", value.Kind))
	}
}

func (d *decoder) unmarshalArray(array ast.Node, v reflect.Value) error {
	assertNode(ast.Array, array)

	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			v.Set(reflect.MakeSlice(v.Type(), 0, 16))
		} else {
			v.SetLen(0)
		}
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() {
			elem = reflect.New(sliceInterfaceType).Elem()
			elem.Set(reflect.MakeSlice(sliceInterfaceType, 0, 16))
		}
		err := d.unmarshalArray(array, elem)
		if err != nil {
			return err
		}
		v.Set(elem)
		return nil
	default:
		// TODO: use newDecodeError, but first the parser needs to fill
		//   array.Data.
		return fmt.Errorf("toml: cannot store array in Go type %s", v.Kind())
	}

	elemType := v.Type().Elem()

	it := array.Children()
	for it.Next() {
		n := it.Node()

		elem := reflect.New(elemType).Elem()

		err := d.handleValue(n, elem)
		if err != nil {
			return err
		}

		v.Set(reflect.Append(v, elem))
	}

	return nil
}

func (d *decoder) unmarshalInlineTable(itable ast.Node, v reflect.Value) error {
	assertNode(ast.InlineTable, itable)

	// Make sure v is an initialized object.
	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	case reflect.Struct:
	// structs are always initialized.
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() {
			elem = reflect.MakeMap(mapStringInterfaceType)
			v.Set(elem)
		}
		return d.unmarshalInlineTable(itable, elem)
	default:
		return newDecodeError(itable.Data, "cannot store inline table in Go type %s", v.Kind())
	}

	it := itable.Children()
	for it.Next() {
		n := it.Node()

		err := d.handleKeyValue(n.Key(), n.Value(), v)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *decoder) unmarshalDateTime(value ast.Node, v reflect.Value) error {
	assertNode(ast.DateTime, value)

	dt, err := parseDateTime(value.Data)
	if err != nil {
		return err
	}

	v.Set(reflect.ValueOf(dt))
	return nil
}

func (d *decoder) unmarshalBool(value ast.Node, v reflect.Value) error {
	assertNode(ast.Bool, value)
	b := value.Data[0] == 't'

	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(b)
	case reflect.Interface:
		v.Set(reflect.ValueOf(b))
	default:
		return newDecodeError(value.Data, "cannot assign boolean to a %t", b)
	}

	return nil
}

func (d *decoder) unmarshalFloat(value ast.Node, v reflect.Value) error {
	assertNode(ast.Float, value)

	f, err := parseFloat(value.Data)
	if err != nil {
		return err
	}

	switch v.Kind() {
	case reflect.Float64:
		v.SetFloat(f)
	case reflect.Float32:
		if f > math.MaxFloat32 {
			return newDecodeError(value.Data, "number %f does not fit in a float32", f)
		}
		v.SetFloat(f)
	case reflect.Interface:
		v.Set(reflect.ValueOf(f))
	default:
		return newDecodeError(value.Data, "float cannot be assigned to %s", v.Kind())
	}

	return nil
}

func (d *decoder) unmarshalInteger(value ast.Node, v reflect.Value) error {
	assertNode(ast.Integer, value)

	const (
		maxInt = int64(^uint(0) >> 1)
		minInt = -maxInt - 1
	)

	i, err := parseInteger(value.Data)
	if err != nil {
		return err
	}

	switch v.Kind() {
	case reflect.Int64:
		v.SetInt(i)
	case reflect.Int32:
		if i < math.MinInt32 || i > math.MaxInt32 {
			return fmt.Errorf("toml: number %d does not fit in an int32", i)
		}

		v.Set(reflect.ValueOf(int32(i)))
		return nil
	case reflect.Int16:
		if i < math.MinInt16 || i > math.MaxInt16 {
			return fmt.Errorf("toml: number %d does not fit in an int16", i)
		}

		v.Set(reflect.ValueOf(int16(i)))
	case reflect.Int8:
		if i < math.MinInt8 || i > math.MaxInt8 {
			return fmt.Errorf("toml: number %d does not fit in an int8", i)
		}

		v.Set(reflect.ValueOf(int8(i)))
	case reflect.Int:
		if i < minInt || i > maxInt {
			return fmt.Errorf("toml: number %d does not fit in an int", i)
		}

		v.Set(reflect.ValueOf(int(i)))
	case reflect.Uint64:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint64", i)
		}

		v.Set(reflect.ValueOf(uint64(i)))
	case reflect.Uint32:
		if i < 0 || i > math.MaxUint32 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint32", i)
		}

		v.Set(reflect.ValueOf(uint32(i)))
	case reflect.Uint16:
		if i < 0 || i > math.MaxUint16 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint16", i)
		}

		v.Set(reflect.ValueOf(uint16(i)))
	case reflect.Uint8:
		if i < 0 || i > math.MaxUint8 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint8", i)
		}

		v.Set(reflect.ValueOf(uint8(i)))
	case reflect.Uint:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint", i)
		}

		v.Set(reflect.ValueOf(uint(i)))
	case reflect.Interface:
		v.Set(reflect.ValueOf(i))
	default:
		err = fmt.Errorf("toml: cannot store TOML integer into a Go %s", v.Kind())
	}

	return err
}

func (d *decoder) unmarshalString(value ast.Node, v reflect.Value) error {
	assertNode(ast.String, value)

	var err error

	switch v.Kind() {
	case reflect.String:
		v.SetString(string(value.Data))
	case reflect.Interface:
		v.Set(reflect.ValueOf(string(value.Data)))
	default:
		err = fmt.Errorf("toml: cannot store TOML string into a Go %s", v.Kind())
	}

	return err
}

func (d *decoder) handleKeyValue(key ast.Iterator, value ast.Node, v reflect.Value) error {
	if key.Next() {
		// Still scoping the key
		return d.handleKeyValuePart(key, value, v)
	}
	// Done scoping the key.
	// v is whatever Go value we need to fill.
	return d.handleValue(value, v)
}

func (d *decoder) handleKeyValuePart(key ast.Iterator, value ast.Node, v reflect.Value) error {
	object := v
	shouldSet := false

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Map:
		// Create the key for the map element. For now assume it's a string.
		mk := reflect.ValueOf(string(key.Node().Data))

		// If the map does not exist, create it.
		if v.IsNil() {
			object = reflect.MakeMap(v.Type())
			shouldSet = true
		}

		mv := object.MapIndex(mk)
		if !mv.IsValid() {
			mv = reflect.New(object.Type().Elem()).Elem()
		}

		err := d.handleKeyValue(key, value, mv)
		if err != nil {
			return err
		}

		object.SetMapIndex(mk, mv)
	case reflect.Struct:
		f, found, err := structField(v, string(key.Node().Data))
		if err != nil || !found {
			return err
		}

		return d.handleKeyValue(key, value, f)
	case reflect.Interface:
		if v.Elem().IsValid() {
			object = v.Elem()
		} else {
			object = reflect.MakeMap(mapStringInterfaceType)
			shouldSet = true
		}

		err := d.handleKeyValuePart(key, value, object)
		if err != nil {
			return err
		}
	default:
		panic(fmt.Errorf("unhandled: %s", v.Kind()))
	}

	if shouldSet {
		v.Set(object)
	}

	return nil
}

func initAndDereferencePointer(v reflect.Value) reflect.Value {
	var elem reflect.Value
	if v.IsNil() {
		elem = reflect.New(v.Type())
		v.Set(elem)
	} else {
		elem = v.Elem()
	}
	return elem
}

type fieldPathsMap = map[string][]int

type fieldPathsCache struct {
	m map[reflect.Type]fieldPathsMap
	l sync.RWMutex
}

func (c *fieldPathsCache) get(t reflect.Type) (fieldPathsMap, bool) {
	c.l.RLock()
	paths, ok := c.m[t]
	c.l.RUnlock()

	return paths, ok
}

func (c *fieldPathsCache) set(t reflect.Type, m fieldPathsMap) {
	c.l.Lock()
	c.m[t] = m
	c.l.Unlock()
}

var globalFieldPathsCache = fieldPathsCache{
	m: map[reflect.Type]fieldPathsMap{},
	l: sync.RWMutex{},
}

func structField(v reflect.Value, name string) (reflect.Value, bool, error) {
	//nolint:godox
	// TODO: cache this, and reduce allocations
	fieldPaths, ok := globalFieldPathsCache.get(v.Type())
	if !ok {
		fieldPaths = map[string][]int{}

		path := make([]int, 0, 16)

		var walk func(reflect.Value)
		walk = func(v reflect.Value) {
			t := v.Type()
			for i := 0; i < t.NumField(); i++ {
				l := len(path)
				path = append(path, i)
				f := t.Field(i)

				if f.Anonymous {
					walk(v.Field(i))
				} else if f.PkgPath == "" {
					// only consider exported fields
					fieldName, ok := f.Tag.Lookup("toml")
					if !ok {
						fieldName = f.Name
					}

					pathCopy := make([]int, len(path))
					copy(pathCopy, path)

					fieldPaths[fieldName] = pathCopy
					// extra copy for the case-insensitive match
					fieldPaths[strings.ToLower(fieldName)] = pathCopy
				}
				path = path[:l]
			}
		}

		walk(v)

		globalFieldPathsCache.set(v.Type(), fieldPaths)
	}

	path, ok := fieldPaths[name]
	if !ok {
		path, ok = fieldPaths[strings.ToLower(name)]
	}

	if !ok {
		return reflect.Value{}, false, nil
	}

	return v.FieldByIndex(path), true, nil
}

func assertNode(expected ast.Kind, node ast.Node) {
	if node.Kind != expected {
		panic(fmt.Sprintf("expected node of kind %s, not %s", expected, node.Kind))
	}
}
func assertSettable(v reflect.Value) {
	if !v.CanAddr() {
		panic(fmt.Errorf("%s is not addressable", v))
	}
	if !v.CanSet() {
		panic(fmt.Errorf("%s is not settable", v))
	}
}

func assertObject(v reflect.Value) {
	switch v.Kind() {
	case reflect.Map:
	case reflect.Struct:
	default:
		panic(fmt.Errorf("expected %s to be a Map or Struct, not %s", v, v.Kind()))
	}
}
