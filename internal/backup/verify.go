package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// Verification is how reading a backup back went.
type Verification struct {
	OK      bool      `json:"ok"`
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	// Checksum says whether a stored checksum was compared (backups from
	// before checksums were kept have none).
	Checksum bool   `json:"checksum"`
	Error    string `json:"error,omitempty"`
}

// verify reads a stored backup back: its checksum (against want, or the
// one stored beside it), its size (if wantSize > 0), its decryption, gzip's
// own checksum, every tar entry, and the file count (if wantFiles >= 0).
// Nothing is written to disk.
func (st *store) verify(ctx context.Context, key string, wantSum string, wantSize int64, wantFiles int) Verification {
	v := Verification{At: time.Now()}
	err := st.check(ctx, key, wantSum, wantSize, wantFiles, &v)
	v.Seconds = time.Since(v.At).Seconds()
	if err != nil {
		v.Error = err.Error()
	} else {
		v.OK = true
	}
	return v
}

func (st *store) check(ctx context.Context, key, wantSum string, wantSize int64, wantFiles int, v *Verification) error {
	if wantSum == "" {
		sum, err := st.sum(ctx, key)
		if err != nil {
			return fmt.Errorf("read the checksum: %w", err)
		}
		wantSum = sum
	}
	rc, _, err := st.cl.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	var stored int64
	raw := io.TeeReader(&countingReader{r: rc, add: func(n int) { stored += int64(n) }}, h)
	plain, err := st.plain(raw)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("damaged after %d files: %w", v.Files, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			n, err := io.Copy(io.Discard, tr)
			if err != nil {
				return fmt.Errorf("%s is damaged: %w", hdr.Name, err)
			}
			v.Files++
			v.Bytes += n
		}
	}
	// The rest: tar's padding, gzip's trailer (which checks its CRC), and
	// anything after, so the checksum covers every byte.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("damaged at the end: %w", err)
	}
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return err
	}
	if wantSize > 0 && stored != wantSize {
		return fmt.Errorf("it's %d bytes, but %d were stored", stored, wantSize)
	}
	if wantSum != "" {
		v.Checksum = true
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
			return errors.New("its checksum doesn't match: it changed after it was stored")
		}
	}
	if wantFiles >= 0 && v.Files != wantFiles {
		return fmt.Errorf("it holds %d files, but %d were backed up", v.Files, wantFiles)
	}
	return nil
}
