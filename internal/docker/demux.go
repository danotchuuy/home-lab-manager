package docker

import (
	"encoding/binary"
	"io"
)

// demuxReader strips the 8-byte frame headers that the daemon puts on
// stdout/stderr streams of containers that have no TTY, and merges the two
// streams into one.
type demuxReader struct {
	r         io.ReadCloser
	remaining uint32 // bytes left in the current frame
	header    [8]byte
}

func newDemuxReader(r io.ReadCloser) *demuxReader {
	return &demuxReader{r: r}
}

func (d *demuxReader) Read(p []byte) (int, error) {
	for d.remaining == 0 {
		if _, err := io.ReadFull(d.r, d.header[:]); err != nil {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			return 0, err
		}
		d.remaining = binary.BigEndian.Uint32(d.header[4:])
	}
	if uint32(len(p)) > d.remaining {
		p = p[:d.remaining]
	}
	n, err := d.r.Read(p)
	d.remaining -= uint32(n)
	if err == io.EOF && d.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (d *demuxReader) Close() error {
	return d.r.Close()
}
