package docker

import (
	"reflect"
	"testing"
)

func TestAddCurrentLabels(t *testing.T) {
	labels := map[string]string{"io.gsh.managed": "true", "io.gsh.app": "old", "io.hlm.app": "new", "other": "x"}
	addCurrentLabels(labels)
	want := map[string]string{"io.gsh.managed": "true", "io.hlm.managed": "true", "io.gsh.app": "old", "io.hlm.app": "new", "other": "x"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %v", labels)
	}
	addCurrentLabels(nil) // no panic
}

func TestLegacyFilters(t *testing.T) {
	if got, ok := legacyFilters([]string{"io.hlm.managed=true", "other"}); !ok || !reflect.DeepEqual(got, []string{"io.gsh.managed=true", "other"}) {
		t.Errorf("filters = %v, %v", got, ok)
	}
	if _, ok := legacyFilters([]string{"other"}); ok {
		t.Error("a filter without io.hlm.* labels has no legacy form")
	}
}
