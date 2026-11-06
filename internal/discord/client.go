package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultAPI is Discord's REST API.
const DefaultAPI = "https://discord.com/api/v10"

// userAgent is what Discord asks bots to send.
var userAgent = "DiscordBot (https://github.com/danotchuuy/home-lab-manager, dev)"

// SetVersion puts hlm's version in the User-Agent.
func SetVersion(v string) {
	userAgent = "DiscordBot (https://github.com/danotchuuy/home-lab-manager, " + v + ")"
}

// Client calls Discord's REST API as a bot.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

// APIError is an error answer from Discord.
type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("discord: %s (HTTP %d)", e.Message, e.Status)
	}
	return fmt.Sprintf("discord: HTTP %d", e.Status)
}

// ErrBadToken is a token Discord doesn't accept.
var ErrBadToken = errors.New("Discord doesn't accept the bot token; copy it again from the developer portal (Bot → Reset Token)")

// snowflakeRE matches Discord IDs.
var snowflakeRE = regexp.MustCompile(`^[0-9]{5,20}$`)

// ValidID reports whether s looks like a Discord ID.
func ValidID(s string) bool { return snowflakeRE.MatchString(s) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.API, "/")+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+c.Token)
		req.Header.Set("User-Agent", userAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		hc := c.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Second}
		}
		resp, err := hc.Do(req)
		if err != nil {
			// Not err itself: it holds the URL, which can hold an
			// interaction token.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("discord: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("discord: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(data, &rl)
			wait := time.Duration(rl.RetryAfter * float64(time.Second))
			if wait <= 0 || wait > 10*time.Second {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return ErrBadToken
		}
		if resp.StatusCode >= 300 {
			var e struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &e)
			return &APIError{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
		}
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		return json.Unmarshal(data, out)
	}
}

// User is a Discord user.
type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name,omitempty"`
	Bot        bool   `json:"bot,omitempty"`
}

// Display is the user's name as people see it.
func (u User) Display() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// Me is the bot's own user.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/users/@me", nil, &u)
	return u, err
}

// Application is the bot's application.
type Application struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// App is the bot's application.
func (c *Client) App(ctx context.Context) (Application, error) {
	var a Application
	err := c.do(ctx, http.MethodGet, "/oauth2/applications/@me", nil, &a)
	return a, err
}

// GatewayURL is where the bot connects.
func (c *Client) GatewayURL(ctx context.Context) (string, error) {
	var g struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodGet, "/gateway/bot", nil, &g); err != nil {
		return "", err
	}
	return g.URL, nil
}

// Role is a role in a guild.
type Role struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Managed  bool   `json:"managed,omitempty"`
	Position int    `json:"position"`
}

// Guild is a Discord server.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Guild looks up a guild the bot is in.
func (c *Client) Guild(ctx context.Context, id string) (Guild, error) {
	if !ValidID(id) {
		return Guild{}, fmt.Errorf("%q isn't a Discord server ID", id)
	}
	var g Guild
	err := c.do(ctx, http.MethodGet, "/guilds/"+id, nil, &g)
	return g, err
}

// Roles lists a guild's roles.
func (c *Client) Roles(ctx context.Context, guild string) ([]Role, error) {
	if !ValidID(guild) {
		return nil, fmt.Errorf("%q isn't a Discord server ID", guild)
	}
	var rs []Role
	err := c.do(ctx, http.MethodGet, "/guilds/"+guild+"/roles", nil, &rs)
	return rs, err
}

// Command is a slash command.
type Command struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Options     []CommandOption `json:"options,omitempty"`
	// DMPermission false keeps it out of direct messages.
	DMPermission *bool `json:"dm_permission,omitempty"`
}

// CommandOption is one of a command's arguments.
type CommandOption struct {
	Type         int    `json:"type"` // 3: string
	Name         string `json:"name"`
	Description  string `json:"description"`
	Required     bool   `json:"required,omitempty"`
	Autocomplete bool   `json:"autocomplete,omitempty"`
	MaxLength    int    `json:"max_length,omitempty"`
}

// SetCommands replaces the bot's commands in a guild (none removes them).
func (c *Client) SetCommands(ctx context.Context, app, guild string, cmds []Command) error {
	if !ValidID(app) || !ValidID(guild) {
		return errors.New("bad application or server ID")
	}
	if cmds == nil {
		cmds = []Command{}
	}
	return c.do(ctx, http.MethodPut, "/applications/"+app+"/guilds/"+guild+"/commands", cmds, nil)
}

// Interaction response types.
const (
	ResponseMessage      = 4 // a reply
	ResponseDeferred     = 5 // "thinking…", followed by EditReply
	ResponseAutocomplete = 8
)

// FlagEphemeral shows a reply only to the person who asked.
const FlagEphemeral = 64

// Message is a reply.
type Message struct {
	Content string  `json:"content,omitempty"`
	Embeds  []Embed `json:"embeds,omitempty"`
	Flags   int     `json:"flags,omitempty"`
	// AllowedMentions with no parse keeps replies from pinging anyone.
	AllowedMentions *struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions,omitempty"`
}

// Embed is a reply's box.
type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
}

// EmbedField is a name and value in an embed.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Choice is an autocomplete suggestion.
type Choice struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func noMentions(m *Message) {
	m.AllowedMentions = &struct {
		Parse []string `json:"parse"`
	}{Parse: []string{}}
}

// Respond answers an interaction.
func (c *Client) Respond(ctx context.Context, id, token string, typ int, data any) error {
	if !ValidID(id) {
		return errors.New("bad interaction ID")
	}
	if m, ok := data.(*Message); ok {
		noMentions(m)
	}
	body := map[string]any{"type": typ}
	if data != nil {
		body["data"] = data
	}
	return c.do(ctx, http.MethodPost, "/interactions/"+id+"/"+url.PathEscape(token)+"/callback", body, nil)
}

// EditReply replaces a deferred reply.
func (c *Client) EditReply(ctx context.Context, app, token string, m Message) error {
	noMentions(&m)
	return c.do(ctx, http.MethodPatch, "/webhooks/"+app+"/"+url.PathEscape(token)+"/messages/@original", m, nil)
}

// InviteURL is the link that adds the bot to a server, with no permissions
// beyond its commands.
func InviteURL(app string) string {
	return "https://discord.com/oauth2/authorize?client_id=" + url.QueryEscape(app) +
		"&scope=" + url.QueryEscape("bot applications.commands") + "&permissions=0"
}
