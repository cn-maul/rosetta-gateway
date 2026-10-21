package admin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestSettingsResponseMirrorsStoreSettings keeps the admin settings DTO and the
// two store-owned settings blocks in lockstep. Adding a runtime setting must
// either expose it here or fail before the frontend silently ignores it.
func TestSettingsResponseMirrorsStoreSettings(t *testing.T) {
	want := make(map[string]reflect.Type)
	addJSONFields(t, want, reflect.TypeOf(store.RuntimeDefaults{}))
	modelFields := map[string]string{
		"ContextWindow":   "default_context_window",
		"MaxOutputTokens": "default_max_output_tokens",
	}
	modelType := reflect.TypeOf(store.ModelDefaults{})
	for i := 0; i < modelType.NumField(); i++ {
		field := modelType.Field(i)
		name, ok := modelFields[field.Name]
		if !ok {
			t.Fatalf("store.ModelDefaults.%s is not mapped to settingsResponse", field.Name)
		}
		delete(modelFields, field.Name)
		want[name] = field.Type
	}
	if len(modelFields) != 0 {
		t.Fatalf("stale settingsResponse model field mappings: %v", modelFields)
	}

	got := make(map[string]reflect.Type)
	addJSONFields(t, got, reflect.TypeOf(settingsResponse{}))

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settingsResponse fields do not mirror store.ModelDefaults + store.RuntimeDefaults:\n got: %v\nwant: %v", got, want)
	}
}

func addJSONFields(t *testing.T, out map[string]reflect.Type, typ reflect.Type) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no usable json tag", typ, field.Name)
		}
		if previous, exists := out[name]; exists && previous != field.Type {
			t.Fatalf("json field %q has conflicting types %s and %s", name, previous, field.Type)
		}
		out[name] = field.Type
	}
}
