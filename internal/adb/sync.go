package adb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Sync file transfer for single files (bulk trees go over exec:tar, §5.2).

func syncConn(ctx context.Context, c *Client, serial string) (net.Conn, error) {
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return nil, err
	}
	if err := request(conn, "sync:"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func writeSyncReq(conn net.Conn, id string, arg string) error {
	if _, err := io.WriteString(conn, id); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(arg)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := io.WriteString(conn, arg)
	return err
}

func readSyncStatus(conn net.Conn) (string, error) {
	var id [4]byte
	if _, err := io.ReadFull(conn, id[:]); err != nil {
		return "", err
	}
	return string(id[:]), nil
}

// SyncSend writes one file to the device with the given mode.
func (c *Client) SyncSend(ctx context.Context, serial, remotePath string, mode uint32, r io.Reader) error {
	conn, err := syncConn(ctx, c, serial)
	if err != nil {
		return err
	}
	defer conn.Close()
	// SEND arg is "path,mode".
	if err := writeSyncReq(conn, "SEND", fmt.Sprintf("%s,%d", remotePath, mode)); err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := io.WriteString(conn, "DATA"); err != nil {
				return err
			}
			var lb [4]byte
			binary.LittleEndian.PutUint32(lb[:], uint32(n))
			if _, err := conn.Write(lb[:]); err != nil {
				return err
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	// DONE + mtime.
	if _, err := io.WriteString(conn, "DONE"); err != nil {
		return err
	}
	var tb [4]byte
	binary.LittleEndian.PutUint32(tb[:], uint32(time.Now().Unix()))
	if _, err := conn.Write(tb[:]); err != nil {
		return err
	}
	status, err := readSyncStatus(conn)
	if err != nil {
		return err
	}
	switch status {
	case "OKAY":
		var skip [4]byte
		_, _ = io.ReadFull(conn, skip[:])
		return nil
	case "FAIL":
		var lb [4]byte
		if _, err := io.ReadFull(conn, lb[:]); err != nil {
			return err
		}
		msg := make([]byte, binary.LittleEndian.Uint32(lb[:]))
		_, _ = io.ReadFull(conn, msg)
		return fmt.Errorf("adb sync send %s: %s", remotePath, strings.TrimSpace(string(msg)))
	default:
		return fmt.Errorf("adb sync send: status %q", status)
	}
}

// SyncRecv reads one file from the device into w.
func (c *Client) SyncRecv(ctx context.Context, serial, remotePath string, w io.Writer) (int64, error) {
	conn, err := syncConn(ctx, c, serial)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := writeSyncReq(conn, "RECV", remotePath); err != nil {
		return 0, err
	}
	var total int64
	for {
		status, err := readSyncStatus(conn)
		if err != nil {
			return total, err
		}
		var lb [4]byte
		if _, err := io.ReadFull(conn, lb[:]); err != nil {
			return total, err
		}
		n := binary.LittleEndian.Uint32(lb[:])
		switch status {
		case "DATA":
			written, err := io.CopyN(w, conn, int64(n))
			total += written
			if err != nil {
				return total, err
			}
		case "DONE":
			return total, nil
		case "FAIL":
			msg := make([]byte, n)
			_, _ = io.ReadFull(conn, msg)
			return total, fmt.Errorf("adb sync recv %s: %s", remotePath, strings.TrimSpace(string(msg)))
		default:
			return total, fmt.Errorf("adb sync recv: status %q", status)
		}
	}
}

// DirEntry is one sync LIST row.
type DirEntry struct {
	Mode uint32
	Size uint32
	Time uint32
	Name string
}

// SyncList lists a device directory.
func (c *Client) SyncList(ctx context.Context, serial, remoteDir string) ([]DirEntry, error) {
	conn, err := syncConn(ctx, c, serial)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := writeSyncReq(conn, "LIST", remoteDir); err != nil {
		return nil, err
	}
	var out []DirEntry
	for {
		status, err := readSyncStatus(conn)
		if err != nil {
			return nil, err
		}
		if status == "DONE" {
			var skip [16]byte
			_, _ = io.ReadFull(conn, skip[:])
			return out, nil
		}
		if status == "FAIL" {
			var lb [4]byte
			_, _ = io.ReadFull(conn, lb[:])
			msg := make([]byte, binary.LittleEndian.Uint32(lb[:]))
			_, _ = io.ReadFull(conn, msg)
			return nil, fmt.Errorf("adb sync list %s: %s", remoteDir, msg)
		}
		if status != "DENT" {
			return nil, fmt.Errorf("adb sync list: status %q", status)
		}
		var fields [16]byte // mode, size, time, nameLen (uint32le each)
		if _, err := io.ReadFull(conn, fields[:]); err != nil {
			return nil, err
		}
		nameLen := binary.LittleEndian.Uint32(fields[12:16])
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(conn, name); err != nil {
			return nil, err
		}
		out = append(out, DirEntry{
			Mode: binary.LittleEndian.Uint32(fields[0:4]),
			Size: binary.LittleEndian.Uint32(fields[4:8]),
			Time: binary.LittleEndian.Uint32(fields[8:12]),
			Name: string(name),
		})
	}
}
