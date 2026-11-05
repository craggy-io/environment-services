package docker

// Network collectors: a collector sidecar (tcpdump/inotify-tools)
// shares the target's network or PID namespace. The sidecar gets
// the extra capabilities and the craggy.* identity labels; the
// target never does. Ports of the old collect package's docker
// mechanisms (net.go, netderived.go, deep.go's HTTP), rewritten to
// the run-once collector contract: no internal retry loops — a dead
// capture returns its error and the core restarts it.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/xfer"
)

//go:embed images/collect/Dockerfile
var sidecarDockerfile []byte

const sidecarImage = "craggy/collect:1"

var sidecarOnce sync.Once
var sidecarErr error

// ensureSidecar builds the collector sidecar image once per process.
func (a *Adapter) ensureSidecar(ctx context.Context) error {
	sidecarOnce.Do(func() {
		if _, err := a.cli.ImageInspect(ctx, sidecarImage); err == nil {
			return
		}
		var buf bytes.Buffer
		src := envspec.MemSource(map[string][]byte{"Dockerfile": sidecarDockerfile})
		if _, err := xfer.WriteTar(ctx, src, &buf, xfer.Options{}); err != nil {
			sidecarErr = err
			return
		}
		res, err := a.cli.ImageBuild(ctx, &buf, client.ImageBuildOptions{Tags: []string{sidecarImage}, Remove: true})
		if err != nil {
			sidecarErr = engineErr(err)
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "\"error\"") {
			sidecarErr = fmt.Errorf("build collector sidecar: %s", lastKB(body))
		}
	})
	return sidecarErr
}

func lastKB(b []byte) string {
	if len(b) > 1024 {
		b = b[len(b)-1024:]
	}
	return string(b)
}

// sidecarExec starts a sidecar sharing this container's net or PID
// namespace and returns its stdout (demuxed) plus a cleanup func.
func (i *Instance) sidecarExec(ctx context.Context, ns string, capAdd []string, argv []string) (io.ReadCloser, func(), error) {
	if err := i.ad.ensureSidecar(ctx); err != nil {
		return nil, nil, err
	}
	host := &container.HostConfig{CapAdd: capAdd, AutoRemove: false}
	switch ns {
	case "net":
		host.NetworkMode = container.NetworkMode("container:" + i.id)
	case "pid":
		host.PidMode = container.PidMode("container:" + i.id)
	}
	created, err := i.ad.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: sidecarImage, Cmd: argv,
			Labels:       map[string]string{LabelSidecar: "collect", LabelEnv: i.envID, LabelManager: i.ad.ManagerID, LabelProfile: i.spec.Profile},
			AttachStdout: true, AttachStderr: true},
		HostConfig: host,
	})
	if err != nil {
		return nil, nil, engineErr(err)
	}
	cleanup := func() {
		_, _ = i.ad.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}
	att, err := i.ad.cli.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		cleanup()
		return nil, nil, engineErr(err)
	}
	if _, err := i.ad.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		att.Close()
		cleanup()
		return nil, nil, engineErr(err)
	}
	pr, pw := io.Pipe()
	go func() {
		// Sidecar stdout may be pcap (binary); stderr frames would
		// corrupt it, so demux by frame header, stdout payload only.
		err := demuxTo(pw, att.Reader)
		_ = pw.CloseWithError(err)
		att.Close()
	}()
	go func() {
		<-ctx.Done()
		cleanup()
	}()
	return pr, cleanup, nil
}

// demuxTo forwards stdout frames of a docker attach stream to w.
func demuxTo(w io.Writer, r io.Reader) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if size == 0 {
			continue
		}
		var dst io.Writer = io.Discard
		if hdr[0] == 1 {
			dst = w
		}
		if _, err := io.CopyN(dst, r, int64(size)); err != nil {
			return err
		}
	}
}

// packetSource starts a sidecar tcpdump in the container's net
// namespace and returns its pcap stream.
func (i *Instance) packetSource(ctx context.Context, iface string) (io.ReadCloser, func(), error) {
	if iface == "" {
		iface = "any"
	}
	return i.sidecarExec(ctx, "net", []string{"NET_RAW", "NET_ADMIN"},
		[]string{"tcpdump", "-i", iface, "-U", "-w", "-", "-s", "0"})
}

type capturedPacket struct {
	summary envspec.PacketData
	packet  gopacket.Packet
}

// streamPackets runs one capture, feeding packets until it ends.
func (i *Instance) streamPackets(ctx context.Context, iface string, ch chan<- capturedPacket) error {
	rc, cleanup, err := i.packetSource(ctx, iface)
	if err != nil {
		return err
	}
	defer cleanup()
	defer rc.Close()
	reader, err := pcapgo.NewReader(rc)
	if err != nil {
		return err
	}
	src := gopacket.NewPacketSource(reader, reader.LinkType())
	src.NoCopy = false
	for {
		pkt, err := src.NextPacket()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		cp := capturedPacket{summary: summarize(pkt), packet: pkt}
		select {
		case ch <- cp:
		case <-ctx.Done():
			return nil
		}
	}
}

// consumePackets runs one capture against fn (the run-once form of
// the old withPacketRetries).
func (i *Instance) consumePackets(ctx context.Context, iface string, fn func(chan capturedPacket)) error {
	ch := make(chan capturedPacket, 256)
	done := make(chan struct{})
	go func() { fn(ch); close(done) }()
	err := i.streamPackets(ctx, iface, ch)
	close(ch)
	<-done
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		return fmt.Errorf("net capture ended")
	}
	return err
}

func summarize(pkt gopacket.Packet) envspec.PacketData {
	d := envspec.PacketData{Length: len(pkt.Data())}
	if ip4 := pkt.Layer(layers.LayerTypeIPv4); ip4 != nil {
		ip := ip4.(*layers.IPv4)
		d.Src, d.Dst, d.Proto = ip.SrcIP.String(), ip.DstIP.String(), ip.Protocol.String()
	} else if ip6 := pkt.Layer(layers.LayerTypeIPv6); ip6 != nil {
		ip := ip6.(*layers.IPv6)
		d.Src, d.Dst, d.Proto = ip.SrcIP.String(), ip.DstIP.String(), ip.NextHeader.String()
	}
	if tcp := pkt.Layer(layers.LayerTypeTCP); tcp != nil {
		t := tcp.(*layers.TCP)
		d.SrcPort, d.DstPort, d.Proto = uint16(t.SrcPort), uint16(t.DstPort), "tcp"
	} else if udp := pkt.Layer(layers.LayerTypeUDP); udp != nil {
		u := udp.(*layers.UDP)
		d.SrcPort, d.DstPort, d.Proto = uint16(u.SrcPort), uint16(u.DstPort), "udp"
	}
	return d
}

func ifaceParams(params json.RawMessage) *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{"iface": strSchema()})
}

func parseIface(params json.RawMessage) string {
	var p struct{ Iface string }
	_ = json.Unmarshal(params, &p)
	return p.Iface
}

// --- net.pcap (bytes) + net.packets@1 ---

type netPCAPCollector struct {
	inst    *Instance
	ChunkKB int
}

func (n *netPCAPCollector) Name() string { return "net.pcap" }
func (n *netPCAPCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{
		{Name: envspec.StreamNetPCAP, Description: "Raw packet capture (classic pcap byte stream)", Bytes: true, Format: "pcap", Cost: "med"},
		{Name: envspec.StreamNetPackets, Description: "Per-packet 5-tuple summaries", Cost: "med", Schema: signalcore.SchemaFor(envspec.PacketData{})},
	}
}
func (n *netPCAPCollector) Params() *jsonschema.Schema { return ifaceParams(nil) }
func (n *netPCAPCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	chunk := n.ChunkKB
	if chunk <= 0 {
		chunk = 64
	}
	rc, cleanup, err := n.inst.packetSource(ctx, parseIface(params))
	if err != nil {
		return fmt.Errorf("net.pcap: start capture: %w", err)
	}
	defer cleanup()
	defer rc.Close()
	// Raw chunks pass through untouched (concatenating them yields
	// the exact capture — redaction never rewrites pcap); a tee
	// feeds the packet summaries.
	pr, pw := io.Pipe()
	go summarizeStream(ctx, pr, out)
	captured := false
	buf := make([]byte, chunk<<10)
	for {
		nr, rerr := rc.Read(buf)
		if nr > 0 {
			captured = true
			chunkBytes := append([]byte(nil), buf[:nr]...)
			out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetPCAP, Data: chunkBytes})
			_, _ = pw.Write(chunkBytes)
		}
		if rerr != nil {
			break
		}
	}
	_ = pw.Close()
	if ctx.Err() != nil {
		return nil
	}
	if !captured {
		return fmt.Errorf("net.pcap: tcpdump exited without producing a capture (missing binary, or no capture permission)")
	}
	return fmt.Errorf("net.pcap: capture ended")
}

// summarizeStream parses a pcap stream into net.packets@1 summaries.
func summarizeStream(ctx context.Context, r io.Reader, out signalcore.Emitter) {
	reader, err := pcapgo.NewReader(r)
	if err != nil {
		return
	}
	src := gopacket.NewPacketSource(reader, reader.LinkType())
	for pkt := range src.Packets() {
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetPackets, Data: summarize(pkt)})
		if ctx.Err() != nil {
			return
		}
	}
}

// --- net.flows@1 / net.dns@1 / net.tls@1 (one capture feeds all) ---

type netDerivedCollector struct {
	inst       *Instance
	FlowWindow time.Duration
}

func (n *netDerivedCollector) Name() string { return "net.derived" }
func (n *netDerivedCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{
		{Name: envspec.StreamNetFlows, Description: "5-tuple flow rollups (bytes per window)", Cost: "low", Schema: signalcore.SchemaFor(envspec.FlowData{})},
		{Name: envspec.StreamNetDNS, Description: "DNS questions and answers", Cost: "low", Schema: signalcore.SchemaFor(envspec.DNSData{})},
		{Name: envspec.StreamNetTLS, Description: "TLS ClientHello SNI + fingerprint", Cost: "low", Schema: signalcore.SchemaFor(envspec.TLSData{})},
	}
}
func (n *netDerivedCollector) Params() *jsonschema.Schema { return ifaceParams(nil) }
func (n *netDerivedCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	win := n.FlowWindow
	if win <= 0 {
		win = 10 * time.Second
	}
	return n.inst.consumePackets(ctx, parseIface(params), func(packets chan capturedPacket) {
		type key struct {
			src, dst, proto string
			sport, dport    uint16
		}
		flows := map[key]*envspec.FlowData{}
		flush := time.NewTicker(win)
		defer flush.Stop()
		for {
			select {
			case cp, ok := <-packets:
				if !ok {
					return
				}
				s := cp.summary
				k := key{s.Src, s.Dst, s.Proto, s.SrcPort, s.DstPort}
				f := flows[k]
				if f == nil {
					f = &envspec.FlowData{Src: s.Src, Dst: s.Dst, Proto: s.Proto, SrcPort: s.SrcPort, DstPort: s.DstPort, Direction: "out"}
					flows[k] = f
				}
				f.Bytes += int64(s.Length)
				f.Packets++
				deriveDNS(cp, out)
				deriveTLS(cp, out)
			case <-flush.C:
				for _, f := range flows {
					out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetFlows, Data: *f})
				}
				flows = map[key]*envspec.FlowData{}
			}
		}
	})
}

func deriveDNS(cp capturedPacket, out signalcore.Emitter) {
	layer := cp.packet.Layer(layers.LayerTypeDNS)
	if layer == nil {
		return
	}
	dns, _ := layer.(*layers.DNS)
	if dns == nil {
		return
	}
	for _, q := range dns.Questions {
		d := envspec.DNSData{QName: string(q.Name), QType: q.Type.String(), Server: cp.summary.Dst}
		for _, a := range dns.Answers {
			switch {
			case a.IP != nil:
				d.Answers = append(d.Answers, a.IP.String())
			case len(a.CNAME) > 0:
				d.Answers = append(d.Answers, string(a.CNAME))
			case len(a.PTR) > 0:
				d.Answers = append(d.Answers, string(a.PTR))
			}
		}
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetDNS, Data: d})
	}
}

// deriveTLS parses a TLS ClientHello from TCP payload for SNI and a
// JA4-style fingerprint (version, cipher/extension counts, ALPN).
func deriveTLS(cp capturedPacket, out signalcore.Emitter) {
	app := cp.packet.ApplicationLayer()
	if app == nil {
		return
	}
	sni, ja4, ok := parseClientHello(app.Payload())
	if !ok {
		return
	}
	out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetTLS, Data: envspec.TLSData{
		Server: cp.summary.Dst, SNI: sni, Port: cp.summary.DstPort, JA4: ja4,
	}})
}

// parseClientHello extracts SNI + fingerprint from a TLS record.
func parseClientHello(b []byte) (sni, ja4 string, ok bool) {
	// TLS record: type(1)=22, ver(2), len(2); handshake: type(1)=1, len(3).
	if len(b) < 5+4+2+32+1 || b[0] != 22 || b[5] != 1 {
		return "", "", false
	}
	r := b[5+4:]
	if len(r) < 34 {
		return "", "", false
	}
	version := uint16(r[0])<<8 | uint16(r[1])
	r = r[2+32:]
	if len(r) < 1 {
		return "", "", false
	}
	sidLen := int(r[0])
	r = r[1:]
	if len(r) < sidLen+2 {
		return "", "", false
	}
	r = r[sidLen:]
	csLen := int(uint16(r[0])<<8 | uint16(r[1]))
	r = r[2:]
	if len(r) < csLen+1 {
		return "", "", false
	}
	ciphers := csLen / 2
	r = r[csLen:]
	compLen := int(r[0])
	r = r[1:]
	if len(r) < compLen+2 {
		return "", "", false
	}
	r = r[compLen:]
	extTotal := int(uint16(r[0])<<8 | uint16(r[1]))
	r = r[2:]
	if len(r) > extTotal {
		r = r[:extTotal]
	}
	extCount := 0
	alpn := ""
	for len(r) >= 4 {
		etype := uint16(r[0])<<8 | uint16(r[1])
		elen := int(uint16(r[2])<<8 | uint16(r[3]))
		r = r[4:]
		if len(r) < elen {
			break
		}
		body := r[:elen]
		extCount++
		switch etype {
		case 0: // server_name
			sni = parseSNI(body)
		case 16: // ALPN
			alpn = parseALPN(body)
		}
		r = r[elen:]
	}
	// JA4-ish compact fingerprint (not byte-identical to FoxIO JA4,
	// but a stable per-client summary).
	ja4 = fmt.Sprintf("t%04x_c%d_e%d_%s", version, ciphers, extCount, alpn)
	return sni, ja4, true
}

func parseSNI(body []byte) string {
	if len(body) < 2 {
		return ""
	}
	body = body[2:]
	for len(body) >= 3 {
		nlen := int(uint16(body[1])<<8 | uint16(body[2]))
		body = body[3:]
		if len(body) < nlen {
			break
		}
		return string(body[:nlen])
	}
	return ""
}

func parseALPN(body []byte) string {
	if len(body) < 3 {
		return ""
	}
	body = body[2:] // list len
	var names []string
	for len(body) >= 1 {
		n := int(body[0])
		body = body[1:]
		if len(body) < n {
			break
		}
		names = append(names, string(body[:n]))
		body = body[n:]
	}
	return strings.Join(names, ",")
}

// --- net.http@1 (plaintext HTTP from the packet stream) ---

type netHTTPCollector struct{ inst *Instance }

func (n *netHTTPCollector) Name() string { return "net.http" }
func (n *netHTTPCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamNetHTTP,
		Description: "HTTP requests/responses (plaintext + egress proxy)", Cost: "med",
		Schema: signalcore.SchemaFor(envspec.HTTPData{})}}
}
func (n *netHTTPCollector) Params() *jsonschema.Schema { return ifaceParams(nil) }
func (n *netHTTPCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	return n.inst.consumePackets(ctx, parseIface(params), func(packets chan capturedPacket) {
		for cp := range packets {
			if d, ok := parseHTTP(cp); ok {
				out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetHTTP, Data: d})
			}
		}
	})
}

var httpMethods = []string{"GET ", "POST ", "PUT ", "DELETE ", "HEAD ", "PATCH ", "OPTIONS ", "CONNECT "}

// parseHTTP recognises requests/responses at a payload start. Full
// TCP reassembly is out of scope for a summary stream; first
// segments carry the interesting metadata.
func parseHTTP(cp capturedPacket) (envspec.HTTPData, bool) {
	app := cp.packet.ApplicationLayer()
	if app == nil {
		return envspec.HTTPData{}, false
	}
	payload := app.Payload()
	if len(payload) < 8 {
		return envspec.HTTPData{}, false
	}
	head := payload
	if idx := bytes.Index(payload, []byte("\r\n\r\n")); idx >= 0 {
		head = payload[:idx]
	} else if len(head) > 8192 {
		head = head[:8192]
	}
	lines := strings.Split(string(head), "\r\n")
	first := strings.TrimSpace(lines[0])
	headers := map[string]string{}
	host := ""
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		headers[strings.TrimSpace(strings.ToLower(k))] = strings.TrimSpace(v)
		if strings.EqualFold(k, "host") {
			host = strings.TrimSpace(v)
		}
	}
	for _, m := range httpMethods {
		if strings.HasPrefix(first, m) {
			parts := strings.Fields(first)
			path := ""
			if len(parts) >= 2 {
				path = parts[1]
			}
			return envspec.HTTPData{Method: strings.TrimSpace(m), Host: host, Path: path,
				Server: cp.summary.Dst, Headers: headers, Dir: "request"}, true
		}
	}
	if strings.HasPrefix(first, "HTTP/") {
		parts := strings.Fields(first)
		code := 0
		if len(parts) >= 2 {
			code, _ = strconv.Atoi(parts[1])
		}
		return envspec.HTTPData{Status: code, Server: cp.summary.Src, Headers: headers, Dir: "response"}, true
	}
	return envspec.HTTPData{}, false
}
