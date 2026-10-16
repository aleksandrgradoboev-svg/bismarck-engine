// Package utf8check finds storage writes that a JSON-backed Go storage cannot
// keep: pi's strings are UTF-16 and keep a lone surrogate as a JSON escape,
// while a Go string holds one only as invalid UTF-8, which encoding/json
// replaces, so distinct identities would read back as one (see
// durabletest.StorageOptions.RejectsInvalidUTF8).
package utf8check

import (
	"encoding/json"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// Writes returns an error naming the storage for the first write holding a
// string, map key or raw JSON payload that is not valid UTF-8.
func Writes(storage string, writes []durable.StorageWrite) error {
	for _, w := range writes {
		if !valid(reflect.ValueOf(w)) {
			return fmt.Errorf("%s cannot keep a string that is not valid UTF-8 (%s write)", storage, w.WriteType())
		}
	}
	return nil
}

func valid(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return utf8.ValidString(v.String())
	case reflect.Pointer, reflect.Interface:
		return v.IsNil() || valid(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if !valid(v.Field(i)) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type() == rawMessageType {
			return utf8.Valid(v.Bytes())
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return true // bytes are encoded as base64
		}
		for i := range v.Len() {
			if !valid(v.Index(i)) {
				return false
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if !valid(it.Key()) || !valid(it.Value()) {
				return false
			}
		}
	}
	return true
}
