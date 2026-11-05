package docker

import (
	"encoding/binary"
	"io"
)

// demuxStdStream splits Docker's multiplexed attach stream into stdout and
// stderr. Frames are an 8-byte header — stream byte (1=stdout, 2=stderr),
// three zero bytes, payload length (big-endian) — followed by the payload.
// (The moby client split stdcopy out; the framing is stable wire format.)
func demuxStdStream(r io.Reader, stdout, stderr io.Writer) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:8])
		w := stdout
		if hdr[0] == 2 {
			w = stderr
		}
		if size == 0 {
			continue
		}
		if _, err := io.CopyN(w, r, int64(size)); err != nil {
			return err
		}
	}
}
