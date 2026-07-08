package secrets

import (
	"fmt"
	"reflect"
)

// secretTag marks struct fields whose values are sensitive and must be
// encrypted at rest: `secret:"true"`. Supported field types are string and
// map[string]string (each map value is treated as sensitive).
const secretTag = "secret"

// EncryptFields walks v (a pointer to a struct) and encrypts every field
// tagged `secret:"true"` in place. Empty and already-encrypted values are
// left untouched, so the operation is idempotent.
func EncryptFields(v any, key []byte) error {
	return walkSecrets(reflect.ValueOf(v), func(s string) (string, error) {
		if s == "" || IsEncrypted(s) {
			return s, nil
		}
		return Encrypt(key, s)
	})
}

// DecryptFields walks v (a pointer to a struct) and decrypts every field
// tagged `secret:"true"` in place. Plaintext values (no enc: prefix) are
// accepted unchanged — this is the transparent migration path for configs
// written before encryption at rest existed.
func DecryptFields(v any, key []byte) error {
	return walkSecrets(reflect.ValueOf(v), func(s string) (string, error) {
		return Decrypt(key, s)
	})
}

// ContainsEncrypted reports whether any field tagged `secret:"true"` in v
// carries an encryption envelope.
func ContainsEncrypted(v any) bool {
	found := false
	_ = walkSecrets(reflect.ValueOf(v), func(s string) (string, error) {
		if IsEncrypted(s) {
			found = true
		}
		return s, nil
	})
	return found
}

type transformFunc func(string) (string, error)

// walkSecrets recursively visits structs, pointers, slices, and maps looking
// for fields tagged `secret:"true"` and applies fn to their values.
func walkSecrets(v reflect.Value, fn transformFunc) error {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return walkSecrets(v.Elem(), fn)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			if sf.Tag.Get(secretTag) == "true" {
				if err := applySecret(field, fn); err != nil {
					return fmt.Errorf("%s.%s: %w", t.Name(), sf.Name, err)
				}
				continue
			}
			if err := walkSecrets(field, fn); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := walkSecrets(v.Index(i), fn); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		for _, k := range v.MapKeys() {
			elem := v.MapIndex(k)
			if !containsStruct(elem.Type()) {
				continue
			}
			// Map values are not addressable: walk an addressable copy and
			// write it back.
			copyVal := reflect.New(elem.Type()).Elem()
			copyVal.Set(elem)
			if err := walkSecrets(copyVal, fn); err != nil {
				return err
			}
			v.SetMapIndex(k, copyVal)
		}
	}
	return nil
}

// applySecret transforms a tagged field value. Supported kinds: string and
// map[string]string.
func applySecret(v reflect.Value, fn transformFunc) error {
	switch {
	case v.Kind() == reflect.String:
		out, err := fn(v.String())
		if err != nil {
			return err
		}
		v.SetString(out)
		return nil
	case v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String && v.Type().Elem().Kind() == reflect.String:
		if v.IsNil() {
			return nil
		}
		for _, k := range v.MapKeys() {
			out, err := fn(v.MapIndex(k).String())
			if err != nil {
				return fmt.Errorf("key %q: %w", k.String(), err)
			}
			v.SetMapIndex(k, reflect.ValueOf(out))
		}
		return nil
	default:
		return fmt.Errorf("unsupported secret field type %s (want string or map[string]string)", v.Type())
	}
}

// containsStruct reports whether t can (transitively) hold struct values that
// may carry secret-tagged fields. Used to skip walking plain map values.
func containsStruct(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct, reflect.Interface:
		return true
	case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map:
		return containsStruct(t.Elem())
	default:
		return false
	}
}
