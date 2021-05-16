package toml

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"reflect"

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
	for d.p.NextExpression() {
		err := d.handleRootExpression(d.p.Expression(), root)
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
		panic("not implemented yet")
	case ast.ArrayTable:
		panic("not implemented yet")
	default:
		panic(fmt.Errorf("parser should not permit expression of kind %s at document root", expr.Kind))
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
	default:
		panic(fmt.Errorf("handleValue not implemented for %s", value.Kind))
	}
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
	} else {
		// Done scoping the key.
		// v is whatever Go value we need to fill.
		return d.handleValue(value, v)
	}
}

func (d *decoder) handleKeyValuePart(key ast.Iterator, value ast.Node, v reflect.Value) error {
	object := v
	shouldSet := false

	// First, dispatch over v to make sure it is a valid object.
	// There is no guarantee over what it could be.
	switch v.Kind() {
	case reflect.Struct:
		// Structs are always valid.
	case reflect.Map:
		mk := reflect.ValueOf(string(key.Node().Data))

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
