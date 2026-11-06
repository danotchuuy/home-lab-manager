package gamequery

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ProbeCommand is the hlm subcommand a probe helper runs: it asks a game,
// by host name and each method in turn, until one answers or time runs out.
const ProbeCommand = "probe-game"

// ProbeOutput is what the probe command prints, as one JSON line.
type ProbeOutput struct {
	Result *Result `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// FormatMethods writes methods for the probe command: "minecraft:25565,a2s:27015".
func FormatMethods(ms []Settings) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Protocol + ":" + strconv.Itoa(m.Port)
	}
	return strings.Join(parts, ",")
}

func parseMethods(s string) ([]Settings, error) {
	var out []Settings
	for _, p := range strings.Split(s, ",") {
		proto, port, ok := strings.Cut(strings.TrimSpace(p), ":")
		n, err := strconv.Atoi(port)
		if !ok || err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("bad method %q", p)
		}
		switch proto {
		case ProtoA2S, ProtoMinecraft, ProtoBedrock, ProtoTCP:
		default:
			return nil, fmt.Errorf("unknown protocol %q", proto)
		}
		out = append(out, Settings{Protocol: proto, Port: n})
	}
	if len(out) == 0 {
		return nil, errors.New("no methods")
	}
	return out, nil
}

var probes = map[string]func(context.Context, string) (Result, error){
	ProtoA2S: A2S, ProtoMinecraft: Minecraft, ProtoBedrock: Bedrock, ProtoTCP: TCP,
}

// ProbeMain runs the probe command: it prints a ProbeOutput and returns
// the exit code (0 when the game answered).
func ProbeMain(args []string, out io.Writer) int {
	fs := flag.NewFlagSet(ProbeCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := fs.String("host", "", "the game's host name or address")
	methods := fs.String("methods", "", "protocol:port, comma-separated")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to keep asking")
	interval := fs.Duration("interval", 5*time.Second, "how long between rounds")
	fail := func(err error) int {
		json.NewEncoder(out).Encode(ProbeOutput{Error: err.Error()})
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	ms, err := parseMethods(*methods)
	if err != nil {
		return fail(err)
	}
	if *host == "" {
		return fail(errors.New("no host"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var last error
	for {
		for _, m := range ms {
			pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
			res, err := probes[m.Protocol](pctx, net.JoinHostPort(*host, strconv.Itoa(m.Port)))
			pcancel()
			if err == nil {
				res.Protocol = m.Protocol
				json.NewEncoder(out).Encode(ProbeOutput{Result: &res})
				return 0
			}
			last = fmt.Errorf("%s port %d: %s", m.Protocol, m.Port, shortError(err))
		}
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			json.NewEncoder(out).Encode(ProbeOutput{Error: last.Error()})
			return 1
		case <-time.After(*interval):
		}
	}
}
