package adb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/internal/adb"
)

// Minimal fake adb server: just enough of the wire protocol for Devices,
// shell v2, and sync send/recv.
type fakeServer struct {
	files  map[string][]byte
	shells []string
}

func readReq(c net.Conn) (string, error) {
	var lb [4]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(string(lb[:]), "%04x", &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(c, buf)
	return string(buf), err
}

func (s *fakeServer) handle(c net.Conn) {
	defer c.Close()
	service, err := readReq(c)
	if err != nil {
		return
	}
	switch {
	case service == "host:version":
		io.WriteString(c, "OKAY00040029")
	case service == "host:devices":
		body := "emulator-5554\tdevice\n"
		fmt.Fprintf(c, "OKAY%04x%s", len(body), body)
	case strings.HasPrefix(service, "host:transport:"):
		io.WriteString(c, "OKAY")
		svc, err := readReq(c)
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(svc, "shell,v2"):
			io.WriteString(c, "OKAY")
			cmd := strings.SplitN(svc, ":", 2)[1]
			s.shells = append(s.shells, cmd)
			// Read stdin frames until close-stdin(4) or a brief timeout.
			_ = c.SetReadDeadline(deadline())
			var stdin bytes.Buffer
			for {
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err != nil {
					break
				}
				n := binary.LittleEndian.Uint32(hdr[1:5])
				payload := make([]byte, n)
				_, _ = io.ReadFull(c, payload)
				if hdr[0] == 4 || hdr[0] != 0 {
					break
				}
				stdin.Write(payload)
			}
			_ = c.SetReadDeadline(noDeadline())
			out, errout, code := "ran:"+cmd, "", byte(0)
			if cmd == "echo ok" {
				out = "ok\n"
			}
			if cmd == "cat" {
				out = stdin.String()
			}
			if strings.HasPrefix(cmd, "fail") {
				errout, code = "bad\n", 7
			}
			writeFrame(c, 1, []byte(out))
			if errout != "" {
				writeFrame(c, 2, []byte(errout))
			}
			writeFrame(c, 3, []byte{code})
		case strings.HasPrefix(svc, "exec:"):
			io.WriteString(c, "OKAY")
			io.WriteString(c, "\x89PNGraw")
		case svc == "sync:":
			io.WriteString(c, "OKAY")
			s.handleSync(c)
		case svc == "root:":
			io.WriteString(c, "OKAYrestarting adbd as root")
		case strings.HasPrefix(svc, "emu:"):
			io.WriteString(c, "OKAYOK\r\n")
		default:
			io.WriteString(c, "FAIL0009no service")
		}
	default:
		io.WriteString(c, "FAIL0007unknown")
	}
}

func writeFrame(c net.Conn, id byte, payload []byte) {
	hdr := make([]byte, 5)
	hdr[0] = id
	binary.LittleEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	c.Write(hdr)
	c.Write(payload)
}

func (s *fakeServer) handleSync(c net.Conn) {
	for {
		id := make([]byte, 4)
		if _, err := io.ReadFull(c, id); err != nil {
			return
		}
		var lb [4]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		n := binary.LittleEndian.Uint32(lb[:])
		switch string(id) {
		case "SEND":
			pathBytes := make([]byte, n)
			_, _ = io.ReadFull(c, pathBytes)
			pathMode := string(pathBytes)
			path, _, _ := strings.Cut(pathMode, ",")
			var data bytes.Buffer
			for {
				did := make([]byte, 4)
				if _, err := io.ReadFull(c, did); err != nil {
					return
				}
				var dl [4]byte
				_, _ = io.ReadFull(c, dl[:])
				dn := binary.LittleEndian.Uint32(dl[:])
				if string(did) == "DONE" {
					break
				}
				chunk := make([]byte, dn)
				_, _ = io.ReadFull(c, chunk)
				data.Write(chunk)
			}
			if s.files == nil {
				s.files = map[string][]byte{}
			}
			s.files[path] = data.Bytes()
			io.WriteString(c, "OKAY")
			c.Write([]byte{0, 0, 0, 0})
			return
		case "RECV":
			pathBytes := make([]byte, n)
			_, _ = io.ReadFull(c, pathBytes)
			data, ok := s.files[string(pathBytes)]
			if !ok {
				io.WriteString(c, "FAIL")
				msg := "no such file"
				var ml [4]byte
				binary.LittleEndian.PutUint32(ml[:], uint32(len(msg)))
				c.Write(ml[:])
				io.WriteString(c, msg)
				return
			}
			io.WriteString(c, "DATA")
			var dl [4]byte
			binary.LittleEndian.PutUint32(dl[:], uint32(len(data)))
			c.Write(dl[:])
			c.Write(data)
			io.WriteString(c, "DONE")
			c.Write([]byte{0, 0, 0, 0})
			return
		case "LIST":
			pathBytes := make([]byte, n)
			_, _ = io.ReadFull(c, pathBytes)
			for path, data := range s.files {
				name := strings.TrimPrefix(path, string(pathBytes)+"/")
				if name == path || strings.Contains(name, "/") {
					continue
				}
				io.WriteString(c, "DENT")
				fields := make([]byte, 16)
				binary.LittleEndian.PutUint32(fields[0:4], 0o100644)
				binary.LittleEndian.PutUint32(fields[4:8], uint32(len(data)))
				binary.LittleEndian.PutUint32(fields[12:16], uint32(len(name)))
				c.Write(fields)
				io.WriteString(c, name)
			}
			io.WriteString(c, "DONE")
			c.Write(make([]byte, 16))
			return
		}
	}
}

func serve(t *testing.T) (*adb.Client, *fakeServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{files: map[string][]byte{"/data/existing.txt": []byte("here")}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fs.handle(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return &adb.Client{Addr: ln.Addr().String()}, fs
}

func TestFakeADBServerProtocol(t *testing.T) {
	client, fs := serve(t)
	ctx := context.Background()
	v, err := client.Version(ctx)
	if err != nil || v != "0029" {
		t.Fatalf("version %q %v", v, err)
	}
	devs, err := client.Devices(ctx)
	if err != nil || len(devs) != 1 || devs[0].Serial != "emulator-5554" || devs[0].State != "device" {
		t.Fatalf("devices %+v %v", devs, err)
	}
	out, code, err := client.Run(ctx, "emulator-5554", "echo ok")
	if err != nil || code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("run %q %d %v", out, code, err)
	}
	// Exit codes and stderr separated by v2 framing.
	p, err := client.Shell(ctx, "emulator-5554", "fail now")
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Stdin.Close()
	go io.Copy(io.Discard, p.Stdout)
	erB, _ := io.ReadAll(p.Stderr)
	code2, _ := p.Wait()
	if code2 != 7 || !strings.Contains(string(erB), "bad") {
		t.Fatalf("fail run: code=%d stderr=%q", code2, erB)
	}
	// stdin frames reach the "device".
	p, err = client.Shell(ctx, "emulator-5554", "cat")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Stdin.Write([]byte("piped input"))
	_ = p.Stdin.Close()
	outB, _ := io.ReadAll(p.Stdout)
	if string(outB) != "piped input" {
		t.Fatalf("cat stdin = %q", outB)
	}
	_, _ = p.Wait()

	// sync send/recv/list against the fake server's store.
	if err := client.SyncSend(ctx, "emulator-5554", "/data/new.txt", 0o100644, strings.NewReader("fresh")); err != nil {
		t.Fatal(err)
	}
	if string(fs.files["/data/new.txt"]) != "fresh" {
		t.Fatalf("server store = %v", fs.files)
	}
	var buf bytes.Buffer
	if _, err := client.SyncRecv(ctx, "emulator-5554", "/data/existing.txt", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "here" {
		t.Fatalf("recv = %q", buf.String())
	}
	entries, err := client.SyncList(ctx, "emulator-5554", "/data")
	if err != nil || len(entries) != 2 {
		t.Fatalf("list = %+v %v", entries, err)
	}
	if _, err := client.Root(ctx, "emulator-5554"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emu(ctx, "emulator-5554", "avd snapshot list"); err != nil {
		t.Fatal(err)
	}
}

func TestQuote(t *testing.T) {
	got := adb.Quote([]string{"sh", "-c", "echo a b; rm -rf /"})
	if strings.Contains(got, "; rm") && !strings.Contains(got, "'") {
		t.Fatalf("quote must neutralise separators: %s", got)
	}
	if adb.Quote([]string{"ls", "/data/local"}) != "ls /data/local" {
		t.Fatalf("simple quote changed safe args")
	}
}
