package files

import "testing"

func TestExcluded(t *testing.T) {
	patterns, err := CheckExclude([]string{" *.log ", "cache/", "/data/steamapps", "mods/*.tmp", ""})
	if err != nil || len(patterns) != 4 {
		t.Fatalf("%v %v", patterns, err)
	}
	for _, tt := range []struct {
		path string
		dir  bool
		want bool
	}{
		{"/data/server.log", false, true},
		{"/data/logs/old.log", false, true},
		{"/data/log", false, false},
		{"/data/cache", true, true},
		{"/data/cache", false, false}, // a file called cache
		{"/data/steamapps", true, true},
		{"/other/steamapps", true, false},
		{"/data/mods/a.tmp", false, true},
		{"/data/x/mods/a.tmp", false, true},
		{"/data/mods/a.jar", false, false},
		{"/data/world/level.dat", false, false},
	} {
		if got := Excluded(patterns, tt.path, tt.dir); got != tt.want {
			t.Errorf("Excluded(%s, dir=%v) = %v", tt.path, tt.dir, got)
		}
	}
	for _, bad := range [][]string{{"["}, {"/"}, {"a\x00"}} {
		if _, err := CheckExclude(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
