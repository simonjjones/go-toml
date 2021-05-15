package toml

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"reflect"
	"time"

	"github.com/pelletier/go-toml/v2/internal/ast"
	"github.com/pelletier/go-toml/v2/internal/tracker"
	"github.com/pelletier/go-toml/v2/internal/unsafe"
)

// Unmarshal deserializes a TOML document into a Go value.
//
// It is a shortcut for Decoder.Decode() with the default options.
func Unmarshal(data []byte, v interface{}) error {
	p := parser{}
	p.Reset(data)
	d := decoder{}

	return d.FromParser(&p, v)
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
		strict: strict{
			Enabled: d.strict,
		},
	}

	return dec.FromParser(&p, v)
}

type decoder struct {
	// Skip expressions until a table is found. This is set to true when a
	// table could not be create (missing field in map), so all KV expressions
	// need to be skipped.
	skipUntilTable bool

	// Tracks position in Go arrays.
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

func (d *decoder) FromParser(p *parser, v interface{}) error {
	err := d.fromParser(p, v)
	if err == nil {
		return d.strict.Error(p.data)
	}

	var e *decodeError
	if errors.As(err, &e) {
		return wrapDecodeError(p.data, e)
	}

	return err
}

func keyLocation(node ast.Node) []byte {
	k := node.Key()

	hasOne := k.Next()
	if !hasOne {
		panic("should not be called with empty key")
	}

	start := k.Node().Data
	end := k.Node().Data

	for k.Next() {
		end = k.Node().Data
	}

	return unsafe.BytesRange(start, end)
}

func (d *decoder) fromParser(p *parser, v interface{}) error {
	r := reflect.ValueOf(v)
	if r.Kind() != reflect.Ptr {
		return fmt.Errorf("toml: decoding can only be performed into a pointer, not %s", r.Kind())
	}

	if r.IsNil() {
		return fmt.Errorf("toml: decoding pointer target cannot be nil")
	}

	root := r.Elem()

	for p.NextExpression() {
		_, err := d.handleExpression(p, root)
		if err != nil {
			return err
		}
	}

	return p.Error()
}

func (d *decoder) handleExpression(p *parser, v reflect.Value) (reflect.Value, error) {
	node := p.Expression()
	switch node.Kind {
	case ast.Table:
		d.skipUntilTable = false
		v, found, err := d.handleTable(p, v)
		d.skipUntilTable = found
		return v, err
	case ast.ArrayTable:
		d.skipUntilTable = false
		panic("todo")
	case ast.KeyValue:
		if d.skipUntilTable {
			return v, nil
		}
		return d.handleKeyValue(p, v)
	default:
		panic(fmt.Errorf("should not be handling a %s node", node.Kind))
	}
}

func (d *decoder) handleKeyValue(p *parser, v reflect.Value) (reflect.Value, error) {
	node := p.Expression()
	assertNode(ast.KeyValue, node)

	k := node.Key()
	if !k.Next() {
		panic("should not have an empty key in a kv")
	}

	return d.scopeKeyValueFast(p, node, k, v)
}

func (d *decoder) scopeKeyValueFast(p *parser, kv ast.Node, key ast.Iterator, v reflect.Value) (reflect.Value, error) {
	if !key.Node().Valid() {
		panic("INVALID NODE")
	}

	part := string(key.Node().Data)

	switch v.Kind() {
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() || elem.Type() != mapStringInterfaceType {
			elem = reflect.MakeMap(mapStringInterfaceType)
		}

		elem, err := d.scopeKeyValueFast(p, kv, key, elem)
		if err != nil {
			return reflect.Value{}, err
		}

		log.Println("=> addr:", v.CanAddr(), "settable:", v.CanSet())
		var newIface interface{}
		v = reflect.ValueOf(&newIface).Elem()
		v.Set(elem)
	case reflect.Map:
		k := reflect.ValueOf(part)

		// check if the element already exists in the map
		elem := v.MapIndex(k)

		// if not allocate a new one
		if !elem.IsValid() || elem.IsNil() {
			elem = reflect.New(v.Type().Elem()).Elem()
		}

		var err error
		if key.Next() {
			elem, err = d.scopeKeyValueFast(p, kv, key, elem)
		} else {
			elem, err = d.decodeKeyValueFast(p, kv.Value(), elem)
		}

		if err != nil {
			return reflect.Value{}, err
		}

		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}

		v.SetMapIndex(k, elem)
	case reflect.Struct:
		f, found, err := fieldOfStruct(v, part)
		if err != nil {
			return reflect.Value{}, err
		}

		if !found {
			return v, nil
		}

		var nv reflect.Value

		if key.Next() {
			nv, err = d.scopeKeyValueFast(p, kv, key, f)
		} else {
			nv, err = d.decodeKeyValueFast(p, kv.Value(), f)
		}

		if err != nil {
			return reflect.Value{}, err
		}

		f.Set(nv)
	default:
		panic(fmt.Errorf("unhandled kind: %s", v.Kind()))
	}

	return v, nil
}

func (d *decoder) decodeKeyValueFast(p *parser, node ast.Node, v reflect.Value) (reflect.Value, error) {
	var err error

	switch node.Kind {
	case ast.Integer:
		err = unmarshalIntegerFast(v, node)
	case ast.String:
		err = unmarshalStringFast(v, node)
	case ast.Float:
		err = unmarshalFloatFast(v, node)
	case ast.Bool:
		err = unmarshalBoolFast(v, node)
	case ast.DateTime:
		err = unmarshalDateTimeFast(v, node)
	case ast.LocalDate:
		err = unmarshalLocalDateFast(v, node)
	case ast.InlineTable:
		err = d.unmarshalInlineTableFast(p, v, node)
	case ast.Array:
		err = d.unmarshalArrayFast(p, v, node)
	default:
		panic(fmt.Errorf("unhandled kind: %s", node.Kind))
	}

	return v, err

	//if x.getKind() == reflect.Ptr {
	//	v := x.get()
	//
	//	if !v.Elem().IsValid() {
	//		x.set(reflect.New(v.Type().Elem()))
	//		v = x.get()
	//	}
	//
	//	return d.unmarshalValue(valueTarget(v.Elem()), node)
	//}
	//
	//ok, err := tryTextUnmarshaler(x, node)
	//if ok {
	//	return err
	//}
	//
	//switch node.Kind {
	//case ast.String:
	//	return unmarshalString(x, node)
	//case ast.Bool:
	//	return unmarshalBool(x, node)
	//case ast.Integer:
	//	return unmarshalInteger(x, node)
	//case ast.Float:
	//	return unmarshalFloat(x, node)
	//case ast.Array:
	//	return d.unmarshalArray(x, node)
	//case ast.InlineTable:
	//	return d.unmarshalInlineTable(x, node)
	//case ast.LocalDateTime:
	//	return unmarshalLocalDateTime(x, node)
	//case ast.DateTime:
	//	return unmarshalDateTime(x, node)
	//case ast.LocalDate:
	//	return unmarshalLocalDate(x, node)
	//default:
	//	panic(fmt.Sprintf("unhandled node kind %s", node.Kind))
	//}
}

func (d *decoder) handleTable(p *parser, v reflect.Value) (reflect.Value, bool, error) {
	node := p.Expression()
	assertNode(ast.Table, node)

	key := node.Key()

	if !key.Next() {
		return reflect.Value{}, false, newDecodeError(node.Data, "empty table keys are not allowed")
	}

	return d.scopeTableFast(p, key, v)
}

func (d *decoder) scopeTableFast(p *parser, key ast.Iterator, v reflect.Value) (reflect.Value, bool, error) {
	if !key.Node().Valid() {
		panic("node is not valid!")
	}

	part := string(key.Node().Data)

	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}

		k := reflect.ValueOf(part)
		// check if the element already exists in the map
		elem := v.MapIndex(k)

		// if not allocate a new one
		created := false
		if !elem.IsValid() {
			created = true
			t := v.Type().Elem()
			if t.Kind() == reflect.Interface {
				t = mapStringInterfaceType
			}
			elem = reflect.New(v.Type().Elem()).Elem()
		}

		var err error
		var found bool
		if !key.Next() {
			// we're done scoping, now we need to get to the next expression.
			if !p.NextExpression() {
				if created {
					v.SetMapIndex(k, elem)
				}
				return elem, true, nil
			}
			elem, err = d.handleExpression(p, elem)
		} else {
			// more parts to of the key to scope
			elem, found, err = d.scopeTableFast(p, key, elem)
		}

		if err != nil || !found {
			return reflect.Value{}, found, err
		}

		v.SetMapIndex(k, elem)
	case reflect.Interface:
		elem := v.Elem()
		if !elem.IsValid() || elem.Type() != mapStringInterfaceType {
			elem = reflect.MakeMap(mapStringInterfaceType)
		}

		var err error
		var found bool
		if !key.Next() {
			// we're done scoping, now we need to get to the next expression.
			if !p.NextExpression() {
				return elem, true, nil
			}
			elem, err = d.handleExpression(p, elem)
		} else {
			// more parts to of the key to scope
			elem, found, err = d.scopeTableFast(p, key, elem)
		}

		if err != nil || !found {
			return reflect.Value{}, found, err
		}

		v.Set(elem)
	case reflect.Struct:
		f, found, err := fieldOfStruct(v, part)
		if err != nil || !found {
			return reflect.Value{}, found, err
		}

		var elem reflect.Value
		if key.Next() {
			// more parts of the key to scope
			elem, found, err = d.scopeTableFast(p, key, f)
		} else {
			// we're done scoping, now we need to get to the next expression.
			if !p.NextExpression() {
				return f, true, nil
			}
			elem, err = d.handleExpression(p, f)
		}

		if err != nil || !found {
			return reflect.Value{}, found, err
		}

		f.Set(elem)

	default:
		panic(fmt.Errorf("unhandled %s", v.Kind()))
	}

	return v, true, nil
}

// scopeWithKey performs target scoping when unmarshaling an ast.KeyValue node.
//
// The goal is to hop from target to target recursively using the names in key.
// Parts of the key should be used to resolve field names for structs, and as
// keys when targeting maps.
//
// When encountering slices, it should always use its last element, and error
// if the slice does not have any.
func (d *decoder) scopeWithKey(x target, key ast.Iterator) (target, bool, error) {
	var (
		err   error
		found bool
	)

	for key.Next() {
		n := key.Node()

		x, found, err = d.scopeTableTarget(false, x, string(n.Data))
		if err != nil || !found {
			return nil, found, err
		}
	}

	return x, true, nil
}

//nolint:cyclop
// scopeWithArrayTable performs target scoping when unmarshaling an
// ast.ArrayTable node.
//
// It is the same as scopeWithKey, but when scoping the last part of the key
// it creates a new element in the array instead of using the last one.
func (d *decoder) scopeWithArrayTable(x target, key ast.Iterator) (target, bool, error) {
	var (
		err   error
		found bool
	)

	for key.Next() {
		n := key.Node()
		if !n.Next().Valid() { // want to stop at one before last
			break
		}

		x, found, err = d.scopeTableTarget(false, x, string(n.Data))
		if err != nil || !found {
			return nil, found, err
		}
	}

	n := key.Node()

	x, found, err = d.scopeTableTarget(false, x, string(n.Data))
	if err != nil || !found {
		return x, found, err
	}

	if x.getKind() == reflect.Ptr {
		x = scopePtr(x)
	}

	if x.getKind() == reflect.Interface {
		x = scopeInterface(true, x)
	}

	switch x.getKind() {
	case reflect.Slice:
		x = scopeSlice(true, x)
	case reflect.Array:
		x, err = d.scopeArray(true, x)
	default:
	}

	return x, found, err
}

func (d *decoder) unmarshalKeyValue(x target, node ast.Node) error {
	assertNode(ast.KeyValue, node)

	d.strict.EnterKeyValue(node)
	defer d.strict.ExitKeyValue(node)

	x, found, err := d.scopeWithKey(x, node.Key())
	if err != nil {
		return err
	}

	// A struct in the path was not found. Skip this value.
	if !found {
		d.strict.MissingField(node)

		return nil
	}

	return d.unmarshalValue(x, node.Value())
}

var textUnmarshalerType = reflect.TypeOf(new(encoding.TextUnmarshaler)).Elem()

func tryTextUnmarshaler(x target, node ast.Node) (bool, error) {
	if x.getKind() != reflect.Struct {
		return false, nil
	}

	v := x.get()

	// Special case for time, because we allow to unmarshal to it from
	// different kind of AST nodes.
	if v.Type() == timeType {
		return false, nil
	}

	if v.Type().Implements(textUnmarshalerType) {
		err := v.Interface().(encoding.TextUnmarshaler).UnmarshalText(node.Data)
		if err != nil {
			return false, newDecodeError(node.Data, "error calling UnmarshalText: %w", err)
		}

		return true, nil
	}

	if v.CanAddr() && v.Addr().Type().Implements(textUnmarshalerType) {
		err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText(node.Data)
		if err != nil {
			return false, newDecodeError(node.Data, "error calling UnmarshalText: %w", err)
		}

		return true, nil
	}

	return false, nil
}

//nolint:cyclop
func (d *decoder) unmarshalValue(x target, node ast.Node) error {
	if x.getKind() == reflect.Ptr {
		v := x.get()

		if !v.Elem().IsValid() {
			x.set(reflect.New(v.Type().Elem()))
			v = x.get()
		}

		return d.unmarshalValue(valueTarget(v.Elem()), node)
	}

	ok, err := tryTextUnmarshaler(x, node)
	if ok {
		return err
	}

	switch node.Kind {
	case ast.String:
		return unmarshalString(x, node)
	case ast.Bool:
		return unmarshalBool(x, node)
	case ast.Integer:
		return unmarshalInteger(x, node)
	case ast.Float:
		return unmarshalFloat(x, node)
	case ast.Array:
		return d.unmarshalArray(x, node)
	case ast.InlineTable:
		return d.unmarshalInlineTable(x, node)
	case ast.LocalDateTime:
		return unmarshalLocalDateTime(x, node)
	case ast.DateTime:
		return unmarshalDateTime(x, node)
	case ast.LocalDate:
		return unmarshalLocalDate(x, node)
	default:
		panic(fmt.Sprintf("unhandled node kind %s", node.Kind))
	}
}

func unmarshalLocalDateFast(v reflect.Value, node ast.Node) error {
	d, err := parseLocalDate(node.Data)
	if err != nil {
		return err
	}

	setDateFast(v, d)

	return nil
}

func unmarshalLocalDate(x target, node ast.Node) error {
	assertNode(ast.LocalDate, node)

	v, err := parseLocalDate(node.Data)
	if err != nil {
		return err
	}

	setDate(x, v)

	return nil
}

func unmarshalLocalDateTime(x target, node ast.Node) error {
	assertNode(ast.LocalDateTime, node)

	v, rest, err := parseLocalDateTime(node.Data)
	if err != nil {
		return err
	}

	if len(rest) > 0 {
		return newDecodeError(rest, "extra characters at the end of a local date time")
	}

	setLocalDateTime(x, v)

	return nil
}

func unmarshalDateTimeFast(v reflect.Value, node ast.Node) error {
	assertNode(ast.DateTime, node)

	t, err := parseDateTime(node.Data)
	if err != nil {
		return err
	}

	v.Set(reflect.ValueOf(t))
	return nil
}

func unmarshalDateTime(x target, node ast.Node) error {
	assertNode(ast.DateTime, node)

	v, err := parseDateTime(node.Data)
	if err != nil {
		return err
	}

	setDateTime(x, v)

	return nil
}

func setLocalDateTime(x target, v LocalDateTime) {
	if x.get().Type() == timeType {
		cast := v.In(time.Local)

		setDateTime(x, cast)
		return
	}

	x.set(reflect.ValueOf(v))
}

func setDateTime(x target, v time.Time) {
	x.set(reflect.ValueOf(v))
}

var timeType = reflect.TypeOf(time.Time{})

func setDateFast(v reflect.Value, d LocalDate) {
	if v.Type() == timeType {
		t := d.In(time.Local)
		v.Set(reflect.ValueOf(t))
		return
	}

	v.Set(reflect.ValueOf(d))
}

func setDate(x target, v LocalDate) {
	if x.get().Type() == timeType {
		cast := v.In(time.Local)

		setDateTime(x, cast)
		return
	}

	x.set(reflect.ValueOf(v))
}

func unmarshalStringFast(v reflect.Value, node ast.Node) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(string(node.Data))
	case reflect.Interface:
		v.Set(reflect.ValueOf(string(node.Data)))
	default:
		return fmt.Errorf("toml: cannot assign string to a %s", v.Kind())
	}
	return nil
}

func unmarshalString(x target, node ast.Node) error {
	assertNode(ast.String, node)

	return setString(x, string(node.Data))
}

func unmarshalBoolFast(v reflect.Value, node ast.Node) error {
	b := node.Data[0] == 't'

	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(b)
	case reflect.Interface:
		v.Set(reflect.ValueOf(b))
	default:
		return fmt.Errorf("toml: cannot assign boolean to a %s", v.Kind())
	}
	return nil
}

func unmarshalBool(x target, node ast.Node) error {
	assertNode(ast.Bool, node)
	v := node.Data[0] == 't'

	return setBool(x, v)
}

func unmarshalIntegerFast(v reflect.Value, node ast.Node) error {
	i, err := parseInteger(node.Data)
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
		v.SetInt(i)
	case reflect.Int16:
		if i < math.MinInt16 || i > math.MaxInt16 {
			return fmt.Errorf("toml: number %d does not fit in an int16", i)
		}
		v.SetInt(i)
	case reflect.Int8:
		if i < math.MinInt8 || i > math.MaxInt8 {
			return fmt.Errorf("toml: number %d does not fit in an int8", i)
		}
		v.SetInt(i)
	case reflect.Int:
		if i < minInt || i > maxInt {
			return fmt.Errorf("toml: number %d does not fit in an int", i)
		}
		v.SetInt(i)
	case reflect.Uint64:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint64", i)
		}
		v.SetUint(uint64(i))
	case reflect.Uint32:
		if i < 0 || i > math.MaxUint32 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint32", i)
		}
		v.SetUint(uint64(i))
	case reflect.Uint16:
		if i < 0 || i > math.MaxUint16 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint16", i)
		}
		v.SetUint(uint64(i))
	case reflect.Uint8:
		if i < 0 || i > math.MaxUint8 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint8", i)
		}
		v.SetUint(uint64(i))
	case reflect.Uint:
		if i < 0 {
			return fmt.Errorf("toml: negative number %d does not fit in an uint", i)
		}
		v.SetUint(uint64(i))
	case reflect.Interface:
		v.Set(reflect.ValueOf(i))
	default:
		return fmt.Errorf("toml: integer cannot be assigned to %s", v.Kind())
	}

	return nil
}

func unmarshalInteger(x target, node ast.Node) error {
	assertNode(ast.Integer, node)

	v, err := parseInteger(node.Data)
	if err != nil {
		return err
	}

	return setInt64(x, v)
}

func unmarshalFloatFast(v reflect.Value, node ast.Node) error {
	f, err := parseFloat(node.Data)
	if err != nil {
		return err
	}

	switch v.Kind() {
	case reflect.Float64:
		v.SetFloat(f)
	case reflect.Float32:
		if f > math.MaxFloat32 {
			return fmt.Errorf("toml: number %f does not fit in a float32", f)
		}
		v.SetFloat(f)
	case reflect.Interface:
		v.Set(reflect.ValueOf(f))
	default:
		return fmt.Errorf("toml: float cannot be assigned to %s", v.Kind())
	}

	return nil
}

func unmarshalFloat(x target, node ast.Node) error {
	assertNode(ast.Float, node)

	v, err := parseFloat(node.Data)
	if err != nil {
		return err
	}

	return setFloat64(x, v)
}

func (d *decoder) unmarshalInlineTableFast(p *parser, v reflect.Value, node ast.Node) error {
	assertNode(ast.InlineTable, node)

	it := node.Children()
	for it.Next() {
		n := it.Node()

		_, err := d.scopeKeyValueFast(p, n, n.Key(), v)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *decoder) unmarshalInlineTable(x target, node ast.Node) error {
	assertNode(ast.InlineTable, node)

	ensureMapIfInterface(x)

	it := node.Children()
	for it.Next() {
		n := it.Node()

		err := d.unmarshalKeyValue(x, n)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *decoder) unmarshalArrayFast(p *parser, v reflect.Value, n ast.Node) error {
	assertNode(ast.Array, n)

	err := ensureValueIndexableFast(v)
	if err != nil {
		return err
	}

}

func (d *decoder) unmarshalArray(x target, node ast.Node) error {
	assertNode(ast.Array, node)

	err := ensureValueIndexable(x)
	if err != nil {
		return err
	}

	// Special work around when unmarshaling into an array.
	// If the array is not addressable, for example when stored as a value in a
	// map, calling elementAt in the inner function would fail.
	// Instead, we allocate a new array that will be filled then inserted into
	// the container.
	// This problem does not exist with slices because they are addressable.
	// There may be a better way of doing this, but it is not obvious to me
	// with the target system.
	if x.getKind() == reflect.Array {
		container := x
		newArrayPtr := reflect.New(x.get().Type())
		x = valueTarget(newArrayPtr.Elem())
		defer func() {
			container.set(newArrayPtr.Elem())
		}()
	}

	return d.unmarshalArrayInner(x, node)
}

func (d *decoder) unmarshalArrayInner(x target, node ast.Node) error {
	idx := 0

	it := node.Children()
	for it.Next() {
		n := it.Node()

		v := elementAt(x, idx)

		if v == nil {
			// when we go out of bound for an array just stop processing it to
			// mimic encoding/json
			break
		}

		err := d.unmarshalValue(v, n)
		if err != nil {
			return err
		}

		idx++
	}
	return nil
}

func assertNode(expected ast.Kind, node ast.Node) {
	if node.Kind != expected {
		panic(fmt.Sprintf("expected node of kind %s, not %s", expected, node.Kind))
	}
}
