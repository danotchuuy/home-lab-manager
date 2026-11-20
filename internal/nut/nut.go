// Package nut reads UPSes from a Network UPS Tools server (upsd): their
// battery charge, runtime, load and whether they're on battery.
package nut

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is upsd's port.
const DefaultPort = "3493"

// Query is where a UPS is: the upsd server, its name there, and the
// account to sign in with (reading a UPS usually needs none).
type Query struct {
	Address  string `json:"address"`
	UPS      string `json:"ups"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Status is a UPS's state, from its variables on upsd.
type Status struct {
	UPS string `json:"ups"`
	// Available is false when it couldn't be read; Reason says why.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	// Flags is ups.status, as upsd gives it: "OL", "OB LB", "OL CHRG".
	Flags      string `json:"flags,omitempty"`
	OnLine     bool   `json:"onLine"`
	OnBattery  bool   `json:"onBattery"`
	LowBattery bool   `json:"lowBattery"`
	Charging   bool   `json:"charging"`
	// Charge and Load are percentages, Runtime seconds; nil when the UPS
	// doesn't report them.
	Charge       *float64  `json:"charge,omitempty"`
	Runtime      *float64  `json:"runtime,omitempty"`
	Load         *float64  `json:"load,omitempty"`
	InputVoltage *float64  `json:"inputVoltage,omitempty"`
	SampledAt    time.Time `json:"sampledAt"`
}

// UPS is a UPS upsd knows.
type UPS struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Fetch reads a UPS's status.
func Fetch(ctx context.Context, q Query) (Status, error) {
	if strings.TrimSpace(q.UPS) == "" {
		return Status{}, errors.New("no UPS name")
	}
	c, err := dial(ctx, q)
	if err != nil {
		return Status{}, err
	}
	defer c.close()
	vars, err := c.list("VAR", q.UPS)
	if err != nil {
		return Status{}, err
	}
	return parseStatus(q.UPS, vars, time.Now().UTC()), nil
}

// List returns the UPSes on the upsd server (q.UPS isn't needed).
func List(ctx context.Context, q Query) ([]UPS, error) {
	c, err := dial(ctx, q)
	if err != nil {
		return nil, err
	}
	defer c.close()
	rows, err := c.list("UPS", "")
	if err != nil {
		return nil, err
	}
	out := make([]UPS, 0, len(rows))
	for _, r := range rows {
		out = append(out, UPS{Name: r.name, Description: r.value})
	}
	return out, nil
}

// Address adds upsd's default port to an address without one.
func Address(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("no NUT server address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]"), DefaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q isn't a host and port (like nas.lan:3493)", addr)
	}
	if host == "" || strings.ContainsAny(host, " /\\@") {
		return "", fmt.Errorf("%q isn't a host name or address", addr)
	}
	return net.JoinHostPort(host, port), nil
}

type conn struct {
	nc net.Conn
	r  *bufio.Reader
}

// timeout bounds a whole conversation with upsd.
const timeout = 10 * time.Second

func dial(ctx context.Context, q Query) (*conn, error) {
	addr, err := Address(q.Address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to the NUT server: %w", err)
	}
	_ = nc.SetDeadline(time.Now().Add(timeout))
	c := &conn{nc: nc, r: bufio.NewReaderSize(nc, 4096)}
	if q.Username != "" {
		if err := c.command("USERNAME " + quote(q.Username)); err != nil {
			c.close()
			return nil, fmt.Errorf("sign in to the NUT server: %w", err)
		}
		if err := c.command("PASSWORD " + quote(q.Password)); err != nil {
			c.close()
			return nil, fmt.Errorf("sign in to the NUT server: %w", err)
		}
	}
	return c, nil
}

func (c *conn) close() {
	_, _ = c.nc.Write([]byte("LOGOUT\n"))
	_ = c.nc.Close()
}

// command sends a line that upsd answers with OK.
func (c *conn) command(line string) error {
	if _, err := c.nc.Write([]byte(line + "\n")); err != nil {
		return err
	}
	resp, err := c.line()
	if err != nil {
		return err
	}
	if err := respErr(resp); err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("unexpected answer %q", resp)
	}
	return nil
}

// maxLine and maxRows bound what's read from upsd.
const (
	maxLine = 1024
	maxRows = 1000
)

func (c *conn) line() (string, error) {
	var b []byte
	for {
		part, more, err := c.r.ReadLine()
		if err != nil {
			return "", err
		}
		b = append(b, part...)
		if len(b) > maxLine {
			return "", errors.New("the NUT server's answer is too long")
		}
		if !more {
			return strings.TrimRight(string(b), "\r"), nil
		}
	}
}

type row struct{ name, value string }

// list runs LIST <what> [ups]: VAR gives a UPS's variables (name, value),
// UPS the server's UPSes (name, description).
func (c *conn) list(what, ups string) ([]row, error) {
	arg := what
	if ups != "" {
		arg += " " + ups
	}
	if strings.ContainsAny(ups, " \t\r\n\"") {
		return nil, errors.New("the UPS name can't contain spaces, quotes or line breaks")
	}
	if _, err := c.nc.Write([]byte("LIST " + arg + "\n")); err != nil {
		return nil, err
	}
	first, err := c.line()
	if err != nil {
		return nil, err
	}
	if err := respErr(first); err != nil {
		if what == "VAR" && errors.Is(err, ErrUnknownUPS) {
			return nil, fmt.Errorf("the NUT server has no UPS called %q", ups)
		}
		return nil, err
	}
	if first != "BEGIN LIST "+arg {
		return nil, fmt.Errorf("unexpected answer %q", first)
	}
	var out []row
	for {
		l, err := c.line()
		if err != nil {
			return nil, err
		}
		if l == "END LIST "+arg {
			return out, nil
		}
		if len(out) >= maxRows {
			return nil, errors.New("the NUT server's list is too long")
		}
		f := fields(l)
		// VAR <ups> <name> "<value>" or UPS <name> "<description>".
		switch {
		case what == "VAR" && len(f) >= 4 && f[0] == "VAR":
			out = append(out, row{f[2], f[3]})
		case what == "UPS" && len(f) >= 3 && f[0] == "UPS":
			out = append(out, row{f[1], f[2]})
		}
	}
}

// ErrUnknownUPS is upsd's answer for a UPS it doesn't have.
var ErrUnknownUPS = errors.New("unknown UPS")

func respErr(line string) error {
	rest, ok := strings.CutPrefix(line, "ERR ")
	if !ok {
		return nil
	}
	code, _, _ := strings.Cut(rest, " ")
	switch code {
	case "UNKNOWN-UPS":
		return ErrUnknownUPS
	case "ACCESS-DENIED":
		return errors.New("the NUT server refused access (check the username and password, or upsd.users)")
	case "INVALID-USERNAME", "INVALID-PASSWORD":
		return errors.New("the NUT server refused the username or password")
	case "DATA-STALE":
		return errors.New("the NUT server's data for this UPS is stale (is its driver running?)")
	case "DRIVER-NOT-CONNECTED":
		return errors.New("the NUT server's driver for this UPS isn't running")
	}
	return fmt.Errorf("the NUT server answered %s", rest)
}

// fields splits a line of upsd's answer into words, unquoting quoted ones.
func fields(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote, escaped, started := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
			started = true
		case r == ' ' && !inQuote:
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

func quote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "", "\r", "").Replace(s)
	return `"` + s + `"`
}

func parseStatus(ups string, vars []row, at time.Time) Status {
	st := Status{UPS: ups, Available: true, SampledAt: at}
	num := func(v string) *float64 {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		return &f
	}
	var mfr string
	for _, v := range vars {
		switch v.name {
		case "ups.status":
			st.Flags = v.value
		case "battery.charge":
			st.Charge = num(v.value)
		case "battery.runtime":
			st.Runtime = num(v.value)
		case "ups.load":
			st.Load = num(v.value)
		case "input.voltage":
			st.InputVoltage = num(v.value)
		case "ups.model", "device.model":
			if st.Model == "" {
				st.Model = strings.TrimSpace(v.value)
			}
		case "ups.mfr", "device.mfr":
			if mfr == "" {
				mfr = strings.TrimSpace(v.value)
			}
		}
	}
	if mfr != "" && !strings.HasPrefix(st.Model, mfr) {
		st.Model = strings.TrimSpace(mfr + " " + st.Model)
	}
	for _, f := range strings.Fields(st.Flags) {
		switch f {
		case "OL":
			st.OnLine = true
		case "OB":
			st.OnBattery = true
		case "LB":
			st.LowBattery = true
		case "CHRG":
			st.Charging = true
		}
	}
	return st
}
