package docker

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs a busybox container that echoes what it reads. Enable with
// HLM_DOCKER_TESTS=1.
func TestDockerWriteStdin(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx := context.Background()
	d, err := NewClient(ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PullImage(ctx, "busybox:1.37", nil); err != nil {
		t.Fatal(err)
	}
	for _, tty := range []bool{false, true} {
		id, _, err := d.CreateContainer(ctx, "", CreateRequest{
			Image: "busybox:1.37", OpenStdin: true, Tty: tty,
			Cmd: []string{"sh", "-c", "while read -r line; do echo \"got: $line\"; done"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.RemoveContainer(context.Background(), id, true) })
		if err := d.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		// Two separate writes: the first attach detaching mustn't close
		// the container's stdin.
		for _, line := range []string{"save-all", "say hello world"} {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := d.WriteStdin(wctx, id, []byte(line+"\n"))
			cancel()
			if err != nil {
				t.Fatalf("tty=%v: %v", tty, err)
			}
		}
		var out string
		for range 50 {
			time.Sleep(100 * time.Millisecond)
			r, err := d.Logs(ctx, id, LogsOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(r)
			r.Close()
			out = string(data)
			if strings.Contains(out, "got: say hello world") {
				break
			}
		}
		if !strings.Contains(out, "got: save-all") || !strings.Contains(out, "got: say hello world") {
			t.Errorf("tty=%v: output = %q", tty, out)
		}
	}

}
