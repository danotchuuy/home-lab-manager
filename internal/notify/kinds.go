package notify

import "time"

// Kind describes a kind of event.
type Kind struct {
	Name        string   `json:"name"`
	Area        string   `json:"area"`
	Severity    Severity `json:"severity"`
	Description string   `json:"description"`
	// Repeat is how long a repeat of the event (same Key) is held back: 0
	// sends every one, and a negative value sends each Key only once.
	Repeat time.Duration `json:"-"`
}

// Event kinds.
const (
	KindServerCrashed     = "server.crashed"
	KindServerUnhealthy   = "server.unhealthy"
	KindUpdateAvailable   = "server.update_available"
	KindServerUpdated     = "server.updated"
	KindServerUpdateFail  = "server.update_failed"
	KindBackupFailed      = "backup.failed"
	KindBackupSucceeded   = "backup.succeeded"
	KindRestoreFinished   = "restore.finished"
	KindRestoreFailed     = "restore.failed"
	KindTunnelDown        = "tunnel.down"
	KindWANChanged        = "wan.changed"
	KindProxyUpdateFailed = "proxy.update_failed"
	KindHTTPSRenewFailed  = "https.renew_failed"
	KindHTTPSExpiring     = "https.expiring"
	KindHLMUpdate         = "hlm.update_available"
	KindHLMUpdateFailed   = "hlm.update_failed"
	KindDiskLow           = "disk.low"
	KindLockedOut         = "auth.locked_out"
	KindTaskFailed        = "task.failed"
	// KindTest is the test message; targets can't choose it.
	KindTest = "notify.test"
)

// sentOnce marks kinds sent only once per Key; their Keys include what
// makes each one new (e.g. the image a server runs).
const sentOnce = -1

// Kinds lists the events targets can choose, in the order the UI shows
// them.
var Kinds = []Kind{
	{KindServerCrashed, "Servers", Error, "A server stopped on its own with an error, or ran out of memory", 15 * time.Minute},
	{KindServerUnhealthy, "Servers", Warning, "A server's health check started failing", time.Hour},
	{KindTaskFailed, "Servers", Error, "A scheduled task failed, or was missed because hlm wasn't running", time.Hour},
	{KindUpdateAvailable, "Updates", Info, "A newer image is available for a server", sentOnce},
	{KindServerUpdated, "Updates", Info, "A server was updated", 0},
	{KindServerUpdateFail, "Updates", Error, "Updating a server failed", 0},
	{KindHLMUpdate, "Updates", Info, "A new version of hlm is available", sentOnce},
	{KindHLMUpdateFailed, "Updates", Error, "hlm couldn't update itself automatically", 0},
	{KindBackupFailed, "Backups", Error, "A backup (of a server or of hlm's configuration) failed", 6 * time.Hour},
	{KindBackupSucceeded, "Backups", Info, "A backup finished", 0},
	{KindRestoreFinished, "Backups", Info, "A restore finished", 0},
	{KindRestoreFailed, "Backups", Error, "A restore failed", 0},
	{KindTunnelDown, "Connections", Error, "The WireGuard tunnel or Cloudflare Tunnel container stopped on its own", 15 * time.Minute},
	{KindWANChanged, "Connections", Info, "The home address changed and the proxy was updated", 0},
	{KindProxyUpdateFailed, "Connections", Error, "The home address changed, but updating the proxy failed", 6 * time.Hour},
	{KindHTTPSRenewFailed, "Host", Error, "Getting or renewing hlm's HTTPS certificate failed", 6 * time.Hour},
	{KindHTTPSExpiring, "Host", Warning, "hlm's HTTPS certificate is getting close to expiring and renewing it keeps failing", 24 * time.Hour},
	{KindDiskLow, "Host", Warning, "A filesystem on the Docker host is nearly full", 24 * time.Hour},
	{KindLockedOut, "Sign-in", Warning, "Sign-in for an account was locked after repeated wrong passwords", time.Hour},
}

// KindByName returns the kind called name.
func KindByName(name string) (Kind, bool) {
	if name == KindTest {
		return Kind{Name: KindTest, Severity: Info}, true
	}
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// DefaultEvents are the events a new target gets: every warning and error.
func DefaultEvents() []string {
	var out []string
	for _, k := range Kinds {
		if k.Severity != Info {
			out = append(out, k.Name)
		}
	}
	return out
}
