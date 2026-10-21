package snapshot

import (
	"reflect"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestSnapshotMirrorsStoreRuntimeDefaults makes the manual copy in rebuild.go
// fail loudly when a runtime setting is added to store.RuntimeDefaults.
func TestSnapshotMirrorsStoreRuntimeDefaults(t *testing.T) {
	storeType := reflect.TypeOf(store.RuntimeDefaults{})
	snapshotType := reflect.TypeOf(RuntimeDefaults{})

	for i := 0; i < storeType.NumField(); i++ {
		storeField := storeType.Field(i)
		snapshotField, ok := snapshotType.FieldByName(storeField.Name)
		if !ok {
			t.Errorf("snapshot.RuntimeDefaults is missing store field %s", storeField.Name)
			continue
		}
		if snapshotField.Type != storeField.Type {
			t.Errorf("snapshot.RuntimeDefaults.%s has type %s, want %s",
				storeField.Name, snapshotField.Type, storeField.Type)
		}
	}
}
