package hosts

import "testing"

func TestLastMessage(t *testing.T) {
	log := `time=2026-11-08T06:35:13.741Z level=INFO msg="stopping hlm-agent"
time=2026-11-08T06:35:15.123Z level=INFO msg="update failed: the new container stopped (exit code 3)"
`
	if got := lastMessage(log); got != "update failed: the new container stopped (exit code 3)" {
		t.Errorf("got %q", got)
	}
	if got := lastMessage("time=x level=INFO msg=done"); got != "done" {
		t.Errorf("unquoted: %q", got)
	}
	if got := lastMessage("plain output\nlast line"); got != "last line" {
		t.Errorf("plain: %q", got)
	}
}
