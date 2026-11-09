package idle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// syncBuffer is written by the listener and read by the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startListener runs a listener and returns a func that waits for what woke
// it (or "" if nothing did within d).
func startListener(t *testing.T, args ...string) func(d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- RunListener(ctx, args, out) }()
	time.Sleep(100 * time.Millisecond) // listening
	return func(d time.Duration) string {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("listener: %v", err)
			}
			return out.String()
		case <-time.After(d):
			return ""
		}
	}
}

func dial(t *testing.T, network string, port int) net.Conn {
	t.Helper()
	c, err := net.Dial(network, "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func handshake(port, next int) []byte {
	data := appendVarInt(nil, 767) // protocol
	data = appendString(data, "localhost")
	data = append(data, byte(port>>8), byte(port))
	data = appendVarInt(data, next)
	return packet(0, data)
}

func TestListenerMinecraft(t *testing.T) {
	mc, tcp, udp, be := freePort(t, "tcp"), freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "udp")
	woke := startListener(t, "--tcp", strconv.Itoa(mc)+","+strconv.Itoa(tcp), "--udp", strconv.Itoa(udp)+","+strconv.Itoa(be),
		"--minecraft", strconv.Itoa(mc), "--bedrock", strconv.Itoa(be), "--max", "20", "--motd", "Asleep; join")

	// A port scan: connect, say nothing, leave.
	dial(t, "tcp", tcp).Close()
	// Steam's server browser.
	dial(t, "udp", udp).Write([]byte("\xff\xff\xff\xffTSource Engine Query\x00"))

	// Minecraft's server list: answered, not a wake.
	c := dial(t, "tcp", mc)
	c.Write(handshake(mc, 1))
	c.Write(packet(0, nil))
	r := bufio.NewReader(c)
	c.SetDeadline(time.Now().Add(3 * time.Second))
	id, body, err := readPacket(r)
	if err != nil || id != 0 {
		t.Fatalf("status: %d %v", id, err)
	}
	n, _ := readVarInt(body)
	status := make([]byte, n)
	body.Read(status)
	var st struct {
		Version     struct{ Protocol int }
		Players     struct{ Max, Online int }
		Description struct{ Text string }
	}
	if err := json.Unmarshal(status, &st); err != nil || st.Version.Protocol != 767 || st.Players.Max != 20 ||
		st.Description.Text != "Asleep; join" {
		t.Errorf("status = %s (%v)", status, err)
	}
	c.Write(packet(1, []byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if id, body, err := readPacket(r); err != nil || id != 1 || body.Len() != 8 {
		t.Errorf("pong: %d %v", id, err)
	}

	// Bedrock's server list.
	u := dial(t, "udp", be)
	ping := append([]byte{0x01, 0, 0, 0, 0, 0, 0, 0, 42}, raknetMagic...)
	u.Write(append(ping, make([]byte, 8)...))
	u.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, err = u.Read(buf)
	if err != nil || buf[0] != 0x1c || buf[8] != 42 || !strings.Contains(string(buf[:n]), "MCPE;Asleep, join;") {
		t.Errorf("bedrock pong: %q %v", buf[:n], err)
	}

	if got := woke(300 * time.Millisecond); got != "" {
		t.Fatalf("woken by a list ping or scan: %s", got)
	}

	// Someone joining.
	c = dial(t, "tcp", mc)
	c.Write(handshake(mc, 2))
	r = bufio.NewReader(c)
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if id, body, err := readPacket(r); err != nil || id != 0 {
		t.Errorf("login answer: %d %v", id, err)
	} else if rest, _ := io.ReadAll(body); !strings.Contains(string(rest), "starting") {
		t.Errorf("login answer: %q", rest)
	}
	var ct Contact
	if err := json.Unmarshal([]byte(woke(3*time.Second)), &ct); err != nil || ct.Port != strconv.Itoa(mc)+"/tcp" ||
		ct.About != "a Minecraft player joining" || !strings.HasPrefix(ct.From, "127.0.0.1:") {
		t.Errorf("contact = %+v (%v)", ct, err)
	}
}

func TestListenerWakes(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		p := freePort(t, "tcp")
		woke := startListener(t, "--tcp", strconv.Itoa(p))
		dial(t, "tcp", p).Write([]byte{0x10})
		if got := woke(3 * time.Second); !strings.Contains(got, `"port":"`+strconv.Itoa(p)+`/tcp"`) {
			t.Errorf("woke = %q", got)
		}
	})
	t.Run("not by scanners", func(t *testing.T) {
		p := freePort(t, "tcp")
		woke := startListener(t, "--tcp", strconv.Itoa(p))
		mongo := make([]byte, 58)
		binary.LittleEndian.PutUint32(mongo[0:], 58)
		binary.LittleEndian.PutUint32(mongo[12:], 2013) // OP_MSG: hello
		for name, b := range map[string][]byte{
			"mongodb": mongo,
			"http":    []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
			"tls":     {0x16, 0x03, 0x01, 0x02, 0x00, 0x01},
			"ssh":     []byte("SSH-2.0-Go\r\n"),
			"rdp":     {0x03, 0x00, 0x00, 0x13, 0x0e, 0xe0},
			"redis":   []byte("*1\r\n$4\r\nINFO\r\n"),
		} {
			c := dial(t, "tcp", p)
			c.Write(b)
			if got := woke(200 * time.Millisecond); got != "" {
				t.Fatalf("woken by %s: %s", name, got)
			}
			c.Close()
		}
		dial(t, "tcp", p).Write([]byte{0x10, 0x00, 0x2f})
		if got := woke(3 * time.Second); got == "" {
			t.Error("a game's own bytes didn't wake it")
		}
	})
	t.Run("udp", func(t *testing.T) {
		p := freePort(t, "udp")
		woke := startListener(t, "--udp", strconv.Itoa(p))
		dial(t, "udp", p).Write([]byte("hello"))
		if got := woke(3 * time.Second); !strings.Contains(got, `"port":"`+strconv.Itoa(p)+`/udp"`) {
			t.Errorf("woke = %q", got)
		}
	})
	t.Run("bedrock join", func(t *testing.T) {
		p := freePort(t, "udp")
		woke := startListener(t, "--udp", strconv.Itoa(p), "--bedrock", strconv.Itoa(p))
		u := dial(t, "udp", p)
		u.Write([]byte("junk"))
		if got := woke(200 * time.Millisecond); got != "" {
			t.Fatalf("woken by junk: %s", got)
		}
		u.Write(append(append([]byte{0x05}, raknetMagic...), 11, 0, 0))
		if got := woke(3 * time.Second); !strings.Contains(got, "Bedrock") {
			t.Errorf("woke = %q", got)
		}
	})
}

func TestListenerArgs(t *testing.T) {
	for _, args := range [][]string{{}, {"--tcp", "0"}, {"--udp", "x"}, {"--tcp", "70000"}} {
		if _, err := parseListenerArgs(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestJoinPorts(t *testing.T) {
	udp := &gameprofile.Profile{JoinProtocols: []string{"udp"}}
	ports := []string{"27017/tcp", "7777/tcp", "7777/udp"}
	if got := joinPorts(udp, ports); !slices.Equal(got, []string{"7777/udp"}) {
		t.Errorf("udp only = %v", got)
	}
	if got := joinPorts(nil, ports); !slices.Equal(got, ports) {
		t.Errorf("no profile = %v", got)
	}
	if got := joinPorts(udp, []string{"25565/tcp"}); !slices.Equal(got, []string{"25565/tcp"}) {
		t.Errorf("none left = %v", got)
	}
}
