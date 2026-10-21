package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestConfigFieldInventoryIsExact makes the configuration surface explicit.
// Any added, deleted, renamed, or retagged JSON field must be reflected in
// configFieldInventory.
func TestConfigFieldInventoryIsExact(t *testing.T) {
	actual, err := collectConfigJSONFields(reflect.TypeOf(Config{}), "")
	if err != nil {
		t.Fatal(err)
	}

	inventory := make(map[string]configFieldSpec, len(configFieldInventory))
	for _, spec := range configFieldInventory {
		if _, exists := inventory[spec.Path]; exists {
			t.Errorf("duplicate inventory entry %q", spec.Path)
			continue
		}
		inventory[spec.Path] = spec
	}

	for path := range actual {
		if _, ok := inventory[path]; !ok {
			t.Errorf("config field %q is missing from configFieldInventory", path)
		}
	}
	for path := range inventory {
		if _, ok := actual[path]; !ok {
			t.Errorf("configFieldInventory entry %q no longer exists in config.Config", path)
		}
	}
}

// TestConfigFieldInventoryReadSitesExist keeps the definition -> read-site
// list from silently pointing at files or symbols removed by later refactors.
func TestConfigFieldInventoryReadSitesExist(t *testing.T) {
	for _, spec := range configFieldInventory {
		if len(spec.ReadBy) == 0 {
			t.Errorf("%s: ReadBy is empty", spec.Path)
		}
		if strings.TrimSpace(spec.Frontend) == "" {
			t.Errorf("%s: Frontend is empty; use %q for startup-only fields", spec.Path, frontendNone)
		}
		for _, ref := range spec.ReadBy {
			path, symbol, ok := strings.Cut(ref, ":")
			if !ok || path == "" || symbol == "" {
				t.Errorf("%s: invalid read reference %q", spec.Path, ref)
				continue
			}
			fullPath := filepath.Join("..", "..", filepath.FromSlash(path))
			src, err := os.ReadFile(fullPath)
			if err != nil {
				t.Errorf("%s: read reference %q: %v", spec.Path, ref, err)
				continue
			}
			if !strings.Contains(string(src), symbol) {
				t.Errorf("%s: read reference %q: symbol %q not found in %s", spec.Path, ref, symbol, path)
			}
		}
	}
}

func collectConfigJSONFields(t reflect.Type, prefix string) (map[string]struct{}, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s: expected struct, got %s", t, t.Kind())
	}

	fields := make(map[string]struct{})
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, ok := jsonFieldName(field)
		if !ok {
			return nil, fmt.Errorf("%s.%s: exported config field must have a json tag", t, field.Name)
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fields[path] = struct{}{}

		fieldType := field.Type
		childPrefix := path
		if fieldType.Kind() == reflect.Slice {
			fieldType = fieldType.Elem()
			childPrefix += "[]"
		}
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() != reflect.Struct {
			continue
		}

		children, err := collectConfigJSONFields(fieldType, childPrefix)
		if err != nil {
			return nil, err
		}
		for child := range children {
			fields[child] = struct{}{}
		}
	}
	return fields, nil
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return "", false
	}
	name := strings.SplitN(tag, ",", 2)[0]
	if name == "" || name == "-" {
		return "", false
	}
	return name, true
}
