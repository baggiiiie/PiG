// Package scalarjson supports JSON-normalized scalar copies without serialization.
package scalarjson

import (
	"math"
	"reflect"
	"unicode/utf8"
)

// CloneFields clones the fields of an already copied struct. Its callers use omitempty maps and explicit empty slices. It reports false for nested data or values JSON rejects or repairs; callers then discard the partial copy and use their serializer. The source struct is never mutated.
func CloneFields(fields reflect.Value) bool {
	for i := range fields.NumField() {
		// Read only values; Fields also constructs unused StructField metadata.
		field := fields.FieldByIndex([]int{i})
		if !field.CanSet() {
			return false
		}
		switch field.Kind() {
		case reflect.String:
			if !utf8.ValidString(field.String()) {
				return false
			}
		case reflect.Pointer:
			if field.IsNil() {
				continue
			}
			value := field.Elem()
			switch value.Kind() {
			case reflect.Bool:
			case reflect.Float64:
				if math.IsNaN(value.Float()) || math.IsInf(value.Float(), 0) {
					return false
				}
			default:
				return false
			}
			pointer := reflect.New(value.Type())
			pointer.Elem().Set(value)
			field.Set(pointer)
		case reflect.Map:
			if field.Len() != 0 {
				return false
			}
			field.SetZero()
		case reflect.Slice:
			if field.Len() != 0 {
				return false
			}
			if !field.IsNil() {
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			}
		default:
			return false
		}
	}
	return true
}
