package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/discord"
)

// --- Discord: a bot for slash commands ---

func (s *server) discordView(w http.ResponseWriter) {
	set, hasToken, err := s.Discord.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": set, "hasToken": hasToken, "state": s.Discord.State(), "inviteUrl": s.Discord.InviteLink(),
		"actions": discord.Actions,
	})
}

func (s *server) discordSettings(w http.ResponseWriter, r *http.Request) { s.discordView(w) }

// saveDiscord stores the settings; a token, if sent, replaces the bot's.
// Rules give people in Discord access, so this takes a confirmed session.
func (s *server) saveDiscord(w http.ResponseWriter, r *http.Request) {
	var in struct {
		discord.Settings
		Token *string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	set := in.Settings
	set.Token = ""
	if _, err := s.Discord.Save(ctx, set, in.Token); err != nil {
		writeDiscordError(w, err)
		return
	}
	s.discordView(w)
}

func (s *server) testDiscord(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.Discord.Test(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": res, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *server) discordRoles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	roles, err := s.Discord.Roles(ctx)
	if err != nil {
		writeDiscordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roles)
}

func (s *server) deleteDiscord(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.Discord.Disconnect(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.discordView(w)
}

func writeDiscordError(w http.ResponseWriter, err error) {
	var ae *discord.APIError
	if errors.As(err, &ae) || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}
