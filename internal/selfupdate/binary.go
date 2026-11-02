package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AssetName is the release file holding the binary for this platform.
func AssetName() string {
	return fmt.Sprintf("hlm_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// ChecksumsAsset lists the SHA-256 of every release file, in sha256sum format.
const ChecksumsAsset = "checksums.txt"

func (u *Updater) applyBinary(ctx context.Context, rel *Release, emit func(Event)) error {
	name := AssetName()
	binURL, ok := rel.assets[name]
	if !ok {
		return fmt.Errorf("release %s has no build for this platform (%s)", rel.Version, name)
	}
	sumURL, ok := rel.assets[ChecksumsAsset]
	if !ok {
		return fmt.Errorf("release %s has no %s, so the download can't be verified", rel.Version, ChecksumsAsset)
	}

	emit(Event{Stage: "download", Message: "Downloading checksums"})
	want, err := u.expectedChecksum(ctx, sumURL, name)
	if err != nil {
		return err
	}

	emit(Event{Stage: "download", Message: fmt.Sprintf("Downloading %s %s", name, rel.Version)})
	dir := filepath.Dir(u.exe)
	tmp, err := os.CreateTemp(dir, ".hlm-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	got, err := u.download(ctx, binURL, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}

	emit(Event{Stage: "verify", Message: "Verifying checksum"})
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}

	emit(Event{Stage: "install", Message: "Installing to " + u.exe})
	backup := u.exe + ".old"
	if err := os.Rename(u.exe, backup); err != nil {
		return fmt.Errorf("back up current binary: %w", err)
	}
	if err := os.Rename(tmp.Name(), u.exe); err != nil {
		_ = os.Rename(backup, u.exe)
		return fmt.Errorf("install new binary: %w", err)
	}
	_ = writeResult(u.resultPath(), Result{From: u.cfg.Version, To: rel.Version, OK: true, At: time.Now()})

	emit(Event{Stage: "restart", Message: fmt.Sprintf("Restarting into %s (the previous binary is kept as %s)", rel.Version, backup)})
	go func() {
		// Give the response time to reach the browser.
		time.Sleep(500 * time.Millisecond)
		if err := u.cfg.Restart(u.exe); err != nil {
			u.cfg.Log.Error("restart after update", "err", err)
			u.mu.Lock()
			u.updating = false
			u.mu.Unlock()
		}
	}()
	return nil
}

func (u *Updater) expectedChecksum(ctx context.Context, url, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums: %s", resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s has no entry for %s", ChecksumsAsset, name)
}

// download writes url to w and returns the hex SHA-256 of what it wrote.
func (u *Updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
