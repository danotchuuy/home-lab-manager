package mods

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultSteamAPI is Steam's Web API.
const DefaultSteamAPI = "https://api.steampowered.com"

// Steam reads Workshop items' public details. No API key is needed.
type Steam struct {
	API       string
	Client    *http.Client
	UserAgent string
}

// NewSteam is a client for Steam's Web API.
func NewSteam(userAgent string) *Steam {
	return &Steam{API: DefaultSteamAPI, Client: &http.Client{Timeout: 20 * time.Second}, UserAgent: userAgent}
}

// WorkshopDetails is what Steam says about a Workshop item.
type WorkshopDetails struct {
	ID      string
	Found   bool
	Title   string
	AppID   int
	Updated time.Time
	Size    int64
	Banned  bool
}

// maxDetails bounds one request.
const maxDetails = 100

// Details looks Workshop items up, by ID.
func (s *Steam) Details(ctx context.Context, ids []string) (map[string]WorkshopDetails, error) {
	out := map[string]WorkshopDetails{}
	for start := 0; start < len(ids); start += maxDetails {
		chunk := ids[start:min(start+maxDetails, len(ids))]
		form := url.Values{"itemcount": {strconv.Itoa(len(chunk))}}
		for i, id := range chunk {
			form.Set(fmt.Sprintf("publishedfileids[%d]", i), id)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(s.API, "/")+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/", strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", s.UserAgent)
		resp, err := s.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("steam: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("steam: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("steam: HTTP %d", resp.StatusCode)
		}
		var body struct {
			Response struct {
				Details []struct {
					ID      string      `json:"publishedfileid"`
					Result  int         `json:"result"`
					Title   string      `json:"title"`
					AppID   int         `json:"consumer_app_id"`
					Updated int64       `json:"time_updated"`
					Size    json.Number `json:"file_size"`
					Banned  int         `json:"banned"`
				} `json:"publishedfiledetails"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, fmt.Errorf("steam: unexpected answer: %w", err)
		}
		for _, d := range body.Response.Details {
			w := WorkshopDetails{ID: d.ID, Found: d.Result == 1, Title: d.Title, AppID: d.AppID, Banned: d.Banned != 0}
			if d.Updated > 0 {
				w.Updated = time.Unix(d.Updated, 0).UTC()
			}
			w.Size, _ = d.Size.Int64()
			out[d.ID] = w
		}
	}
	return out, nil
}

var (
	workshopIDRE   = regexp.MustCompile(`^[0-9]{1,20}$`)
	workshopLinkRE = regexp.MustCompile(`[?&]id=([0-9]{1,20})\b`)
	// modNameRE is a mod name in a list hlm writes.
	modNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.'()+-]{0,99}$`)
)

// WorkshopID reads an item's ID, or its Workshop link.
func WorkshopID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if workshopIDRE.MatchString(s) {
		return s, true
	}
	if m := workshopLinkRE.FindStringSubmatch(s); m != nil &&
		(strings.Contains(s, "steamcommunity.com/sharedfiles/") || strings.Contains(s, "steamcommunity.com/workshop/")) {
		return m[1], true
	}
	return "", false
}
