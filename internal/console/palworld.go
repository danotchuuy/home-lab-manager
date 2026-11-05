package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/reach"
)

// Palworld's REST API (https://docs.palworldgame.com/category/rest-api)
// replaces its RCON. It's served on RESTAPIPort (8212 by default) under
// /v1/api, with HTTP basic auth: user "admin" and the server's
// AdminPassword. hlm turns the console commands people know from RCON
// (Save, Broadcast, ShowPlayers, …) into its calls.

// DefaultRESTPort is Palworld's usual REST API port.
const DefaultRESTPort = 8212

const (
	restUser   = "admin"
	restPrefix = "/v1/api"
)

// restPortVars are environment variables that commonly hold the REST
// API's port.
var restPortVars = []string{"REST_API_PORT", "RESTAPI_PORT", "RESTAPIPORT"}

// restEnabledVars turn the REST API on in Palworld images.
var restEnabledVars = []string{"REST_API_ENABLED", "RESTAPI_ENABLED", "RESTAPIENABLED"}

// restPasswordVars hold the admin password, best first.
var restPasswordVars = []string{"ADMIN_PASSWORD", "ADMINPASSWORD", "SERVER_ADMIN_PASSWORD", "SERVER_ADMINPASSWORD"}

// restCommand is a console command as a REST call.
type restCommand struct {
	method, path string
	body         any
	// format turns the answer into the console's reply.
	format func(data []byte) (string, error)
}

// RESTCommands lists the commands the REST console takes, for help.
var RESTCommands = []string{
	"Info", "Metrics", "ShowPlayers", "Settings", "Save", "Broadcast <message>",
	"KickPlayer <userId> [message]", "BanPlayer <userId> [message]", "UnBanPlayer <userId>",
	"Shutdown <seconds> [message]", "DoExit",
}

func done(msg string) func([]byte) (string, error) {
	return func([]byte) (string, error) { return msg, nil }
}

// parseRESTCommand turns a console command into a REST call. Commands are
// RCON's names, case-insensitive, with a few plain-word aliases.
func parseRESTCommand(command string) (restCommand, error) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(command), " ")
	rest = strings.TrimSpace(rest)
	// The first word of rest, and what follows it.
	arg, tail, _ := strings.Cut(rest, " ")
	tail = strings.TrimSpace(tail)
	switch strings.ToLower(verb) {
	case "info":
		return restCommand{method: http.MethodGet, path: "/info", format: formatInfo}, nil
	case "metrics":
		return restCommand{method: http.MethodGet, path: "/metrics", format: formatMetrics}, nil
	case "showplayers", "players":
		return restCommand{method: http.MethodGet, path: "/players", format: formatPlayers}, nil
	case "settings":
		return restCommand{method: http.MethodGet, path: "/settings", format: formatJSON}, nil
	case "save":
		return restCommand{method: http.MethodPost, path: "/save", format: done("The world was saved.")}, nil
	case "broadcast", "announce":
		if rest == "" {
			return restCommand{}, errors.New("Broadcast needs a message: Broadcast <message>")
		}
		return restCommand{method: http.MethodPost, path: "/announce", body: map[string]string{"message": rest},
			format: done("Announced: " + rest)}, nil
	case "kickplayer", "kick", "banplayer", "ban":
		what := "kick"
		if strings.HasPrefix(strings.ToLower(verb), "ban") {
			what = "ban"
		}
		if arg == "" {
			return restCommand{}, fmt.Errorf("%s needs the player's user ID (from ShowPlayers): %s <userId> [message]", verb, verb)
		}
		body := map[string]string{"userid": arg}
		if tail != "" {
			body["message"] = tail
		}
		past := map[string]string{"kick": "Kicked", "ban": "Banned"}[what]
		return restCommand{method: http.MethodPost, path: "/" + what, body: body, format: done(past + " " + arg + ".")}, nil
	case "unbanplayer", "unban":
		if arg == "" {
			return restCommand{}, errors.New("UnBanPlayer needs the player's user ID: UnBanPlayer <userId>")
		}
		return restCommand{method: http.MethodPost, path: "/unban", body: map[string]string{"userid": arg},
			format: done("Unbanned " + arg + ".")}, nil
	case "shutdown":
		secs, err := strconv.Atoi(arg)
		if err != nil || secs < 0 {
			return restCommand{}, errors.New("Shutdown needs the seconds to wait: Shutdown <seconds> [message]")
		}
		body := map[string]any{"waittime": secs}
		if tail != "" {
			body["message"] = tail
		}
		return restCommand{method: http.MethodPost, path: "/shutdown", body: body,
			format: done(fmt.Sprintf("The server shuts down in %d seconds.", secs))}, nil
	case "doexit", "stop":
		return restCommand{method: http.MethodPost, path: "/stop", format: done("The server was stopped.")}, nil
	}
	return restCommand{}, fmt.Errorf("Palworld's REST API doesn't take %q; it takes %s", verb, strings.Join(RESTCommands, ", "))
}

func formatInfo(data []byte) (string, error) {
	var v struct {
		Version     string `json:"version"`
		ServerName  string `json:"servername"`
		Description string `json:"description"`
		WorldGUID   string `json:"worldguid"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("unexpected answer: %w", err)
	}
	out := fmt.Sprintf("Server: %s\nVersion: %s", v.ServerName, v.Version)
	if v.Description != "" {
		out += "\nDescription: " + v.Description
	}
	if v.WorldGUID != "" {
		out += "\nWorld: " + v.WorldGUID
	}
	return out, nil
}

// Metrics is the answer to GET /metrics.
type Metrics struct {
	ServerFPS       int     `json:"serverfps"`
	Players         int     `json:"currentplayernum"`
	ServerFrameTime float64 `json:"serverframetime"`
	MaxPlayers      int     `json:"maxplayernum"`
	Uptime          int     `json:"uptime"`
	BaseCamps       int     `json:"basecampnum"`
	Days            int     `json:"days"`
}

func formatMetrics(data []byte) (string, error) {
	var m Metrics
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("unexpected answer: %w", err)
	}
	return fmt.Sprintf("Players: %d/%d\nServer FPS: %d (frame time %.1f ms)\nUptime: %s\nIn-game day: %d\nBase camps: %d",
		m.Players, m.MaxPlayers, m.ServerFPS, m.ServerFrameTime, time.Duration(m.Uptime)*time.Second, m.Days, m.BaseCamps), nil
}

// Player is a player in the answer to GET /players.
type Player struct {
	Name          string  `json:"name"`
	AccountName   string  `json:"accountName"`
	PlayerID      string  `json:"playerId"`
	UserID        string  `json:"userId"`
	IP            string  `json:"ip"`
	Ping          float64 `json:"ping"`
	X             float64 `json:"location_x"`
	Y             float64 `json:"location_y"`
	Level         int     `json:"level"`
	BuildingCount int     `json:"building_count"`
}

// formatPlayers lists players the way RCON's ShowPlayers did
// (name,playeruid,steamid), so what reads that keeps working, with the
// level and ping after.
func formatPlayers(data []byte) (string, error) {
	var v struct {
		Players []Player `json:"players"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("unexpected answer: %w", err)
	}
	var b strings.Builder
	b.WriteString("name,playeruid,steamid,level,ping")
	for _, p := range v.Players {
		name := strings.ReplaceAll(p.Name, ",", " ")
		fmt.Fprintf(&b, "\n%s,%s,%s,%d,%.0f", name, p.PlayerID, p.UserID, p.Level, p.Ping)
	}
	return b.String(), nil
}

func formatJSON(data []byte) (string, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return string(data), nil
	}
	return buf.String(), nil
}

// restPort finds the REST API's port in the container: from settings, a
// variable that sets it, or the usual one.
func restPort(info docker.ContainerJSON, set Settings) int {
	if set.Port != 0 && set.Host == "" {
		return set.Port
	}
	env := envMap(info.Config.Env)
	for _, v := range restPortVars {
		if n, err := strconv.Atoi(strings.TrimSpace(env[v])); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return DefaultRESTPort
}

// restAddresses lists where a server's REST API might be reached from hlm,
// best first.
func (s *Service) restAddresses(info docker.ContainerJSON, set Settings) ([]string, error) {
	if set.Host != "" {
		return []string{net.JoinHostPort(set.Host, strconv.Itoa(set.Port))}, nil
	}
	out := reach.Addresses(info, restPort(info, set), "tcp", s.opts.HostAddresses())
	if len(out) == 0 {
		return nil, errors.New("the REST API's port can't be reached: the container has no address hlm can use, and the port isn't published")
	}
	return out, nil
}

// errRESTAuth is returned when the REST API turns the password down.
var errRESTAuth = errors.New("the REST API turned the admin password down (401): check the password, which is the server's AdminPassword")

// sendREST runs a console command as a REST call.
func (s *Service) sendREST(ctx context.Context, info docker.ContainerJSON, set Settings, command string) (string, error) {
	cmd, err := parseRESTCommand(command)
	if err != nil {
		return "", err
	}
	data, err := s.restCall(ctx, info, set, cmd.method, cmd.path, cmd.body)
	if err != nil {
		return "", err
	}
	return cmd.format(data)
}

// restCall makes one REST call, trying each address the API might be at,
// starting with the one that worked last.
func (s *Service) restCall(ctx context.Context, info docker.ContainerJSON, set Settings, method, path string, body any) ([]byte, error) {
	password := passwordOf(info, set)
	if password == "" {
		return nil, errors.New("there's no admin password: choose the variable that holds it, or enter it, in the console settings")
	}
	addrs, err := s.restAddresses(info, set)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	last := s.restLast[info.ID]
	s.mu.Unlock()
	if i := slices.Index(addrs, last); i > 0 {
		addrs = append([]string{last}, slices.Delete(addrs, i, i+1)...)
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var errs []string
	for _, a := range addrs {
		data, err := s.restOnce(ctx, a, password, method, path, payload)
		if err == nil {
			s.mu.Lock()
			s.restLast[info.ID] = a
			s.mu.Unlock()
			return data, nil
		}
		var he *restHTTPError
		if errors.Is(err, errRESTAuth) || errors.As(err, &he) {
			return nil, err // it answered: another address won't do better
		}
		errs = append(errs, fmt.Sprintf("%s: %v", a, err))
	}
	return nil, errors.New("couldn't reach Palworld's REST API (" + strings.Join(errs, "; ") + "); is RESTAPIEnabled=True in the server's settings?")
}

type restHTTPError struct {
	status int
	body   string
}

func (e *restHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("the REST API answered %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("the REST API answered %d", e.status)
}

func (s *Service) restOnce(ctx context.Context, addr, password, method, path string, payload []byte) ([]byte, error) {
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(actx, method, "http://"+addr+restPrefix+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(restUser, password)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, shortNetError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errRESTAuth
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &restHTTPError{status: resp.StatusCode, body: truncate(strings.TrimSpace(string(data)), 200)}
	}
	return data, nil
}

// shortNetError turns Go's long network errors into a few words.
func shortNetError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return errors.New("nothing is listening on that port")
	case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "i/o timeout"):
		return errors.New("no answer")
	}
	return err
}

// testREST checks the REST API answers with this password, and says what
// it found.
func (s *Service) testREST(ctx context.Context, info docker.ContainerJSON, set Settings) (string, error) {
	data, err := s.restCall(ctx, info, set, http.MethodGet, "/info", nil)
	if err != nil {
		return "", err
	}
	out, err := formatInfo(data)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	addr := s.restLast[info.ID]
	s.mu.Unlock()
	return "Signed in to Palworld's REST API at " + addr + ". " + strings.ReplaceAll(out, "\n", " · "), nil
}
