package gamequery

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestProbeMain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	var out bytes.Buffer
	// The first method fails; the second answers.
	code := ProbeMain([]string{"--host", "127.0.0.1", "--methods", "minecraft:1," + FormatMethods([]Settings{{Protocol: ProtoTCP, Port: port}}),
		"--interval", "10ms", "--timeout", "5s"}, &out)
	var got ProbeOutput
	json.Unmarshal(out.Bytes(), &got)
	if code != 0 || got.Result == nil || got.Result.Protocol != ProtoTCP {
		t.Fatalf("answered: %d %s", code, out.String())
	}

	out.Reset()
	ln.Close()
	code = ProbeMain([]string{"--host", "127.0.0.1", "--methods", "tcp:" + strconv.Itoa(port), "--interval", "10ms", "--timeout", "100ms"}, &out)
	json.Unmarshal(out.Bytes(), &got)
	if code != 1 || !strings.Contains(got.Error, "tcp port") {
		t.Errorf("timeout: %d %s", code, out.String())
	}
	for _, args := range [][]string{{"--host", "x", "--methods", "rcon:1"}, {"--host", "x", "--methods", "tcp:0"}, {"--methods", "tcp:1"}} {
		out.Reset()
		if code := ProbeMain(args, &out); code != 2 {
			t.Errorf("%v: %d %s", args, code, out.String())
		}
	}
}
