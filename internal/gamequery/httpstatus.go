package gamequery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProtoHTTPStatus is a game's HTTP status page: Nightingale's /status,
// which answers JSON with "status" ("loading", then "ready"),
// "player_count" and "player_names".
const ProtoHTTPStatus = "http-status"

// ErrLoading is a status page saying the game isn't ready for players yet.
var ErrLoading = errors.New("the game is still loading")

// httpStatusClient never follows redirects or uses a proxy: the page is on
// the LAN.
var httpStatusClient = &http.Client{
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// HTTPStatus asks a game's HTTP status page at addr.
func HTTPStatus(ctx context.Context, addr string) (Result, error) {
	start := time.Now()
	ctx, cancel := context.WithDeadline(ctx, deadline(ctx, 3*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/status", nil)
	if err != nil {
		return Result{}, err
	}
	resp, err := httpStatusClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return Result{}, err
	}
	st, err := parseHTTPStatus(body)
	switch {
	case err != nil && resp.StatusCode == http.StatusServiceUnavailable:
		return Result{}, ErrLoading
	case err != nil:
		return Result{}, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	case resp.StatusCode == http.StatusServiceUnavailable || (st.Status != "" && st.Status != "ready"):
		return Result{}, ErrLoading
	case resp.StatusCode != http.StatusOK:
		return Result{}, fmt.Errorf("the status page answered HTTP %d", resp.StatusCode)
	}
	res := Result{Protocol: ProtoHTTPStatus, Address: addr, Latency: time.Since(start), Players: st.Count, HasPlayers: true}
	for _, n := range st.Names {
		if n = strings.TrimSpace(n); n != "" && len(res.PlayerNames) < maxPlayerNames {
			res.PlayerNames = append(res.PlayerNames, n)
		}
	}
	if res.Players < len(res.PlayerNames) {
		res.Players = len(res.PlayerNames)
	}
	return res, nil
}

type httpStatus struct {
	Status string   `json:"status"`
	Count  int      `json:"player_count"`
	Names  []string `json:"player_names"`
}

var (
	statusField = regexp.MustCompile(`"status"\s*:\s*"([^"]*)"`)
	countField  = regexp.MustCompile(`"player_count"\s*:\s*(\d+)`)
	namesField  = regexp.MustCompile(`(?s)"player_names"\s*:\s*\[(.*?)\]`)
	jsonString  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

// parseHTTPStatus reads a status page: JSON, or near enough (Nightingale's
// documentation shows it without commas between the fields).
func parseHTTPStatus(body []byte) (httpStatus, error) {
	var st httpStatus
	if err := json.Unmarshal(body, &st); err == nil {
		return st, nil
	}
	s := string(body)
	m := statusField.FindStringSubmatch(s)
	if m == nil {
		return st, errors.New(`not a status page (no "status")`)
	}
	st.Status = m[1]
	if c := countField.FindStringSubmatch(s); c != nil {
		st.Count, _ = strconv.Atoi(c[1])
	}
	if n := namesField.FindStringSubmatch(s); n != nil {
		for _, q := range jsonString.FindAllString(n[1], -1) {
			var name string
			if json.Unmarshal([]byte(q), &name) == nil {
				st.Names = append(st.Names, name)
			}
		}
	}
	return st, nil
}
