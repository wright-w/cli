package structdiff

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/databricks/cli/libs/structs/structaccess"
	"github.com/databricks/cli/libs/structs/structpath"
	"github.com/databricks/cli/libs/structs/structtag"
)

type Change struct {
	Path *structpath.PathNode
	Old  any
	New  any
}

// KeyFunc extracts a key field name and value from a slice element.
// It can be either:
//   - func(T) (string, string) - typed function for specific element type T
//   - func(any) (string, string) - generic function accepting any element
//
// The function returns (keyField, keyValue). The keyField is typically a field name
// like "task_key", and keyValue is the value that uniquely identifies the element.
type KeyFunc = any

// keyFuncCaller wraps a KeyFunc and provides a type-checked Call method.
type keyFuncCaller struct {
	fn      reflect.Value
	argType reflect.Type
}

func newKeyFuncCaller(fn any) (*keyFuncCaller, error) {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return nil, fmt.Errorf("KeyFunc must be a function, got %T", fn)
	}
	t := v.Type()
	if t.NumIn() != 1 {
		return nil, fmt.Errorf("KeyFunc must have exactly 1 parameter, got %d", t.NumIn())
	}
	if t.NumOut() != 2 {
		return nil, fmt.Errorf("KeyFunc must return exactly 2 values, got %d", t.NumOut())
	}
	if t.Out(0).Kind() != reflect.String || t.Out(1).Kind() != reflect.String {
		return nil, fmt.Errorf("KeyFunc must return (string, string), got (%v, %v)", t.Out(0), t.Out(1))
	}
	return &keyFuncCaller{fn: v, argType: t.In(0)}, nil
}

func (c *keyFuncCaller) call(elem any) (string, string) {
	elemValue := reflect.ValueOf(elem)
	out := c.fn.Call([]reflect.Value{elemValue})
	keyField := out[0].String()
	keyValue := out[1].String()
	return keyField, keyValue
}

// diffContext holds configuration and state for the diff operation.
type diffContext struct {
	sliceKeys map[string]KeyFunc

	// path is the current position as a stack of segments. A PathNode is only
	// allocated when a change is recorded there, since most visited values are equal.
	path    []pathSegment
	changes []Change

	// mapKeys is a scratch buffer for diffStringMaps.
	mapKeys []string
}

// Contexts are reused so the path stack and key buffer are not reallocated on every diff.
var diffContextPool = sync.Pool{New: func() any { return &diffContext{} }}

func (ctx *diffContext) release() {
	// Drop references to the caller's values so the pool does not retain them.
	clear(ctx.path[:cap(ctx.path)])
	clear(ctx.mapKeys[:cap(ctx.mapKeys)])
	*ctx = diffContext{path: ctx.path[:0], mapKeys: ctx.mapKeys[:0]}
	diffContextPool.Put(ctx)
}

type segmentKind int

const (
	segmentIndex segmentKind = iota
	segmentDotString
	segmentBracketString
	segmentKeyValue
)

type pathSegment struct {
	kind  segmentKind
	index int
	key   string
	value string

	// node caches the materialized PathNode. It stays valid while the segment is
	// on the stack, because the segments below it cannot change until it is popped.
	node *structpath.PathNode
}

func (ctx *diffContext) push(s pathSegment) {
	ctx.path = append(ctx.path, s)
}

func (ctx *diffContext) pop() {
	ctx.path = ctx.path[:len(ctx.path)-1]
}

// pathNode materializes the current path, reusing nodes already built for
// earlier changes under the same prefix.
func (ctx *diffContext) pathNode() *structpath.PathNode {
	// Cached nodes always form a prefix of the stack, so only the segments after
	// the last cached one need building.
	start := len(ctx.path)
	for start > 0 && ctx.path[start-1].node == nil {
		start--
	}
	var prev *structpath.PathNode
	if start > 0 {
		prev = ctx.path[start-1].node
	}
	for i := start; i < len(ctx.path); i++ {
		s := &ctx.path[i]
		switch s.kind {
		case segmentIndex:
			s.node = structpath.NewIndex(prev, s.index)
		case segmentDotString:
			s.node = structpath.NewDotString(prev, s.key)
		case segmentBracketString:
			s.node = structpath.NewBracketString(prev, s.key)
		case segmentKeyValue:
			s.node = structpath.NewKeyValue(prev, s.key, s.value)
		}
		prev = s.node
	}
	return prev
}

func (ctx *diffContext) addChange(oldValue, newValue any) {
	ctx.changes = append(ctx.changes, Change{Path: ctx.pathNode(), Old: oldValue, New: newValue})
}

// GetStructDiff compares two Go structs and returns a list of Changes or an error.
// Respects ForceSendFields if present.
// Types of a and b must match exactly, otherwise returns an error.
//
// The sliceKeys parameter maps path patterns to functions that extract
// key field/value pairs from slice elements. When provided, slices at matching
// paths are compared as maps keyed by (keyField, keyValue) instead of by index.
// Path patterns use dot notation (e.g., "tasks" or "job.tasks").
// The [*] wildcard matches any slice index in the path.
// Note, key wildcard is not supported yet ("a.*.c")
// Pass nil if no slice key functions are needed.
func GetStructDiff(a, b any, sliceKeys map[string]KeyFunc) ([]Change, error) {
	v1 := reflect.ValueOf(a)
	v2 := reflect.ValueOf(b)

	if !v1.IsValid() && !v2.IsValid() {
		return nil, nil
	}

	var changes []Change

	if !v1.IsValid() || !v2.IsValid() {
		changes = append(changes, Change{Path: nil, Old: v1.Interface(), New: v2.Interface()})
		return changes, nil
	}

	if v1.Type() != v2.Type() {
		return nil, fmt.Errorf("type mismatch: %v vs %v", v1.Type(), v2.Type())
	}

	ctx := diffContextPool.Get().(*diffContext)
	defer ctx.release()
	ctx.sliceKeys = sliceKeys
	if err := diffValues(ctx, v1, v2); err != nil {
		return nil, err
	}
	return ctx.changes, nil
}

// diffValues appends changes between v1 and v2 to ctx.changes, at the current ctx.path.
func diffValues(ctx *diffContext, v1, v2 reflect.Value) error {
	if !v1.IsValid() {
		if !v2.IsValid() {
			return nil
		}
		// Dereference non-nil pointers for consistency with the both-non-nil case,
		// where pointers are recursively dereferenced via "case reflect.Pointer".
		for v2.Kind() == reflect.Pointer && !v2.IsNil() {
			v2 = v2.Elem()
		}
		ctx.addChange(nil, v2.Interface())
		return nil
	} else if !v2.IsValid() {
		// v1 is valid
		for v1.Kind() == reflect.Pointer && !v1.IsNil() {
			v1 = v1.Elem()
		}
		ctx.addChange(v1.Interface(), nil)
		return nil
	}

	v1Type := v1.Type()

	// This should not happen; if it does, record this a full change
	if v1Type != v2.Type() {
		ctx.addChange(v1.Interface(), v2.Interface())
		return nil
	}

	kind := v1.Kind()

	// Perform nil checks for nilable types.
	switch kind {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan, reflect.Func:
		v1Nil := v1.IsNil()
		v2Nil := v2.IsNil()
		if v1Nil && v2Nil {
			return nil
		}
		if v1Nil || v2Nil {
			ctx.addChange(v1.Interface(), v2.Interface())
			return nil
		}
	default:
		// Not a nilable type.
		// Proceed with direct comparison below.
	}

	switch kind {
	case reflect.Pointer:
		return diffValues(ctx, v1.Elem(), v2.Elem())
	case reflect.Struct:
		if IsOpaqueStruct(v1Type) {
			equal, err := equalJSON(v1, v2)
			if err != nil {
				return err
			}
			if !equal {
				ctx.addChange(v1.Interface(), v2.Interface())
			}
			return nil
		}
		return diffStruct(ctx, v1, v2)
	case reflect.Slice, reflect.Array:
		if keyFunc := ctx.findKeyFunc(); keyFunc != nil {
			return diffSliceByKey(ctx, v1, v2, keyFunc)
		} else if v1.Len() != v2.Len() {
			ctx.addChange(v1.Interface(), v2.Interface())
		} else {
			for i := range v1.Len() {
				ctx.push(pathSegment{kind: segmentIndex, index: i})
				err := diffValues(ctx, v1.Index(i), v2.Index(i))
				ctx.pop()
				if err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if v1Type.Key().Kind() == reflect.String {
			return diffMapStringKey(ctx, v1, v2)
		} else {
			diffLeaf(ctx, v1, v2)
		}
	default:
		diffLeaf(ctx, v1, v2)
	}
	return nil
}

func diffLeaf(ctx *diffContext, v1, v2 reflect.Value) {
	if !equalLeaf(v1, v2) {
		ctx.addChange(v1.Interface(), v2.Interface())
	}
}

// equalLeaf compares values that are not walked into. Basic kinds are compared
// directly: reflect.DeepEqual needs Interface(), which allocates for each of them.
func equalLeaf(v1, v2 reflect.Value) bool {
	switch v1.Kind() {
	case reflect.String:
		return v1.String() == v2.String()
	case reflect.Bool:
		return v1.Bool() == v2.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v1.Int() == v2.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v1.Uint() == v2.Uint()
	case reflect.Float32, reflect.Float64:
		return v1.Float() == v2.Float()
	default:
		return reflect.DeepEqual(v1.Interface(), v2.Interface())
	}
}

func diffStruct(ctx *diffContext, s1, s2 reflect.Value) error {
	info := getStructInfo(s1.Type())
	forced1 := info.getForceSendFields(s1)
	forced2 := info.getForceSendFields(s2)

	for _, f := range info.fields {
		v1Field := s1.Field(f.index)
		v2Field := s2.Field(f.index)

		// Continue traversing embedded structs. Do not add the key to the path though.
		if f.flattened {
			if err := diffValues(ctx, v1Field, v2Field); err != nil {
				return err
			}
			continue
		}

		// `json:"-,"` names the field "-" for encoding/json, but the diff has always skipped it.
		if f.dashName {
			continue
		}

		if f.omitEmpty {
			if v1Field.IsZero() && !slices.Contains(forced1, f.goName) {
				v1Field = reflect.ValueOf(nil)
			}
			if v2Field.IsZero() && !slices.Contains(forced2, f.goName) {
				v2Field = reflect.ValueOf(nil)
			}
		}

		// EmbeddedSlice: diff at parent path level without adding field name.
		if f.embeddedSlice {
			if err := diffValues(ctx, v1Field, v2Field); err != nil {
				return err
			}
			continue
		}

		ctx.push(pathSegment{kind: segmentDotString, key: f.name})
		err := diffValues(ctx, v1Field, v2Field)
		ctx.pop()
		if err != nil {
			return err
		}
	}
	return nil
}

func diffMapStringKey(ctx *diffContext, m1, m2 reflect.Value) error {
	if s1, ok := reflect.TypeAssert[map[string]string](m1); ok {
		s2, _ := reflect.TypeAssert[map[string]string](m2)
		diffStringMaps(ctx, s1, s2)
		return nil
	}

	keySet := map[string]reflect.Value{}
	for _, k := range m1.MapKeys() {
		// Caller guarantees the key kind is String; use Value.String() rather
		// than a .(string) assertion, which panics on a named string key type
		// (e.g. `type ScriptHook string`) whose dynamic type is not string.
		keySet[k.String()] = k
	}
	for _, k := range m2.MapKeys() {
		keySet[k.String()] = k
	}

	keys := slices.Sorted(maps.Keys(keySet))

	for _, ks := range keys {
		k := keySet[ks]
		v1 := m1.MapIndex(k)
		v2 := m2.MapIndex(k)
		ctx.push(pathSegment{kind: segmentBracketString, key: ks})
		err := diffValues(ctx, v1, v2)
		ctx.pop()
		if err != nil {
			return err
		}
	}
	return nil
}

// diffStringMaps is diffMapStringKey for map[string]string, the most common map
// type in configs (tags, spark_conf, ...). Going through reflect would copy every
// key and value to the heap.
func diffStringMaps(ctx *diffContext, m1, m2 map[string]string) {
	keys := ctx.mapKeys[:0]
	for k := range m1 {
		keys = append(keys, k)
	}
	for k := range m2 {
		if _, ok := m1[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	ctx.mapKeys = keys

	for _, k := range keys {
		s1, ok1 := m1[k]
		s2, ok2 := m2[k]
		if ok1 && ok2 && s1 == s2 {
			continue
		}
		ctx.push(pathSegment{kind: segmentBracketString, key: k})
		switch {
		case !ok1:
			ctx.addChange(nil, s2)
		case !ok2:
			ctx.addChange(s1, nil)
		default:
			ctx.addChange(s1, s2)
		}
		ctx.pop()
	}
}

// structField is the per-type part of diffing a struct field, cached in structInfo.
type structField struct {
	index int
	// name is the JSON key, or the Go field name when the tag has none.
	name   string
	goName string

	omitEmpty bool
	dashName  bool

	// flattened marks an anonymous struct that encoding/json promotes into the parent.
	// An anonymous field carrying a json name is not one of these: encoding/json
	// serializes it as a nested object, so it is handled as a named field.
	flattened bool

	embeddedSlice bool
}

type structInfo struct {
	// fields holds the exported fields to walk, in declaration order. ForceSendFields
	// and fields tagged exactly `json:"-"` are left out.
	fields []structField

	// forceSendFieldsIndex locates ForceSendFields (possibly promoted from an
	// embedded struct); nil if the struct has no []string field of that name.
	forceSendFieldsIndex []int
}

// Keyed by reflect.Type; parsing struct tags for every visited field dominated the diff.
var structInfoCache sync.Map

func getStructInfo(t reflect.Type) *structInfo {
	if cached, ok := structInfoCache.Load(t); ok {
		return cached.(*structInfo)
	}
	info := newStructInfo(t)
	structInfoCache.Store(t, info)
	return info
}

func newStructInfo(t reflect.Type) *structInfo {
	info := &structInfo{}

	if sf, ok := t.FieldByName("ForceSendFields"); ok && sf.Type == reflect.TypeFor[[]string]() {
		info.forceSendFieldsIndex = sf.Index
	}

	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() || sf.Name == "ForceSendFields" {
			continue
		}

		if structaccess.IsFlattenedEmbed(sf) {
			info.fields = append(info.fields, structField{index: i, flattened: true})
			continue
		}

		if structaccess.IsSkippedField(sf) {
			continue
		}

		jsonTag := structtag.JSONTag(sf.Tag.Get("json"))

		// Resolve field name from JSON tag or fall back to Go field name
		fieldName := jsonTag.Name()

		isEmbed := sf.Name == structaccess.EmbeddedSliceFieldName

		if fieldName == "" || isEmbed {
			fieldName = sf.Name
		}

		info.fields = append(info.fields, structField{
			index:         i,
			name:          fieldName,
			goName:        sf.Name,
			omitEmpty:     jsonTag.OmitEmpty(),
			dashName:      jsonTag.Name() == "-",
			embeddedSlice: isEmbed,
		})
	}
	return info
}

func (info *structInfo) getForceSendFields(v reflect.Value) []string {
	if info.forceSendFieldsIndex == nil {
		return nil
	}
	result, _ := reflect.TypeAssert[[]string](v.FieldByIndex(info.forceSendFieldsIndex))
	return result
}

// findKeyFunc returns the KeyFunc for the current path, or nil if none matches.
// Path patterns support [*] to match any slice index.
func (ctx *diffContext) findKeyFunc() KeyFunc {
	if ctx.sliceKeys == nil {
		return nil
	}
	return ctx.sliceKeys[ctx.pathPattern()]
}

// pathPattern converts the current path to a pattern string for matching.
// Slice indices and key-value pairs are converted to [*] wildcard.
func (ctx *diffContext) pathPattern() string {
	var result strings.Builder

	for i, s := range ctx.path {
		switch s.kind {
		case segmentIndex, segmentKeyValue:
			result.WriteString("[*]")
		case segmentDotString, segmentBracketString:
			if i != 0 {
				result.WriteString(".")
			}
			result.WriteString(s.key)
		}
	}

	return result.String()
}

// sliceElement holds a slice element with its key information.
type sliceElement struct {
	keyField string
	keyValue string
	value    reflect.Value
}

// validateKeyFuncElementType verifies that the first element type in the sequence
// is assignable to the expected type. If the sequence is empty, it succeeds.
func validateKeyFuncElementType(seq reflect.Value, expected reflect.Type) error {
	if seq.Len() == 0 {
		return nil
	}
	elem := seq.Index(0)
	if !elem.Type().AssignableTo(expected) {
		return fmt.Errorf("KeyFunc expects %v, got %v", expected, elem.Type())
	}
	return nil
}

// diffSliceByKey compares two slices using the provided key function.
// Elements are matched by their (keyField, keyValue) pairs instead of by index.
// Duplicate keys are allowed and matched in order.
func diffSliceByKey(ctx *diffContext, v1, v2 reflect.Value, keyFunc KeyFunc) error {
	caller, err := newKeyFuncCaller(keyFunc)
	if err != nil {
		return err
	}

	// Validate element types up-front to avoid runtime panics and to return a clear error.
	if err := validateKeyFuncElementType(v1, caller.argType); err != nil {
		return err
	}
	if err := validateKeyFuncElementType(v2, caller.argType); err != nil {
		return err
	}

	// Build lists of elements grouped by key, preserving order within each key
	elements1 := make(map[string][]sliceElement)
	elements2 := make(map[string][]sliceElement)
	seen := make(map[string]bool)
	var orderedKeys []string

	// Build from first slice
	for i := range v1.Len() {
		elem := v1.Index(i)
		keyField, keyValue := caller.call(elem.Interface())
		elements1[keyValue] = append(elements1[keyValue], sliceElement{keyField: keyField, keyValue: keyValue, value: elem})
		if !seen[keyValue] {
			seen[keyValue] = true
			orderedKeys = append(orderedKeys, keyValue)
		}
	}

	// Build from second slice
	for i := range v2.Len() {
		elem := v2.Index(i)
		keyField, keyValue := caller.call(elem.Interface())
		elements2[keyValue] = append(elements2[keyValue], sliceElement{keyField: keyField, keyValue: keyValue, value: elem})
		if !seen[keyValue] {
			seen[keyValue] = true
			orderedKeys = append(orderedKeys, keyValue)
		}
	}

	// Compare elements by key in original order
	for _, keyValue := range orderedKeys {
		list1 := elements1[keyValue]
		list2 := elements2[keyValue]

		var keyField string
		if len(list1) > 0 {
			keyField = list1[0].keyField
		} else {
			keyField = list2[0].keyField
		}

		ctx.push(pathSegment{kind: segmentKeyValue, key: keyField, value: keyValue})
		err := diffKeyGroup(ctx, list1, list2)
		ctx.pop()
		if err != nil {
			return err
		}
	}
	return nil
}

// diffKeyGroup diffs the elements of both slices that share one key, at the current path.
func diffKeyGroup(ctx *diffContext, list1, list2 []sliceElement) error {
	// Match elements in order
	minLen := min(len(list1), len(list2))
	for i := range minLen {
		if err := diffValues(ctx, list1[i].value, list2[i].value); err != nil {
			return err
		}
	}

	// Handle extra elements in old (deleted)
	for i := minLen; i < len(list1); i++ {
		ctx.addChange(list1[i].value.Interface(), nil)
	}

	// Handle extra elements in new (added)
	for i := minLen; i < len(list2); i++ {
		ctx.addChange(nil, list2[i].value.Interface())
	}
	return nil
}
