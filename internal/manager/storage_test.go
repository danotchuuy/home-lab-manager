package manager

import "testing"

func TestTotalWithoutNested(t *testing.T) {
	folders := []DataFolder{
		{Path: "/srv/minecraft", Target: "/data", Size: 1000},
		{Path: "/srv/minecraft/downloads", Target: "/downloads", Size: 300},
		{Path: "/srv/minecraft-backups", Target: "/backups", Size: 50}, // a sibling, not nested
		{Path: "world", Target: "/world", Volume: true, Size: 7},
		{Path: "/srv/unknown", Target: "/x", Size: -1},
	}
	if got := totalWithoutNested(folders); got != 1057 {
		t.Errorf("total = %d, want 1057", got)
	}
	if folders[1].Within != "/data" || folders[2].Within != "" {
		t.Errorf("within = %q, %q", folders[1].Within, folders[2].Within)
	}
}
