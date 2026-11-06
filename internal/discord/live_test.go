package discord

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGatewayLive connects to Discord's real gateway with a token it
// refuses (HLM_LIVE_DISCORD=1): the WebSocket, TLS, hello and close codes
// work, and a refused token stops the bot rather than retrying.
func TestGatewayLive(t *testing.T) {
	if os.Getenv("HLM_LIVE_DISCORD") != "1" {
		t.Skip("set HLM_LIVE_DISCORD=1 to talk to Discord")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := &gateway{client: &Client{API: DefaultAPI, Token: "MTAwMDAwMDAwMDAwMDAwMDAw.not.areal-token"},
		url: "wss://gateway.discord.gg", log: slog.New(slog.DiscardHandler)}
	err := g.connect(ctx)
	var fatal *FatalError
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "doesn't accept the bot token") {
		t.Fatalf("connect: %v", err)
	}
	if _, err := (&Client{API: DefaultAPI, Token: "nope"}).Me(ctx); !errors.Is(err, ErrBadToken) {
		t.Errorf("me: %v", err)
	}
}
