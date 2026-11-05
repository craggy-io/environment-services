package androidemu

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Host-side ramdisk surgery for Magisk. The on-device route dies on
// modern images: the API 36 ramdisk is LZ4-*legacy* framed, which
// magiskboot's `cpio` refuses to load ("invalid cpio magic") and its
// `decompress` refuses as input. So the patch — the exact entry changes
// boot_patch.sh performs — happens here on the parsed newc archive,
// with a re-parse verification before anything is written.

type ramdiskFormat int

const (
	ramdiskRaw ramdiskFormat = iota
	ramdiskGzip
	ramdiskXZ
	ramdiskLZ4Legacy
	ramdiskLZ4Frame
)

var (
	lz4LegacyMagic = []byte{0x02, 0x21, 0x4C, 0x18}
	lz4FrameMagic  = []byte{0x04, 0x22, 0x4D, 0x18}
)

func sniffRamdisk(data []byte) (ramdiskFormat, error) {
	switch {
	case len(data) < 6:
		return 0, fmt.Errorf("android: ramdisk too small (%d bytes)", len(data))
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return ramdiskGzip, nil
	case bytes.HasPrefix(data, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		return ramdiskXZ, nil
	case bytes.HasPrefix(data, lz4LegacyMagic):
		return ramdiskLZ4Legacy, nil
	case bytes.HasPrefix(data, lz4FrameMagic):
		return ramdiskLZ4Frame, nil
	case bytes.HasPrefix(data, []byte("070701")) || bytes.HasPrefix(data, []byte("070702")):
		return ramdiskRaw, nil
	default:
		return 0, fmt.Errorf("android: unsupported ramdisk format (header % x)", data[:6])
	}
}

func decompressRamdisk(data []byte) ([]byte, ramdiskFormat, error) {
	format, err := sniffRamdisk(data)
	if err != nil {
		return nil, 0, err
	}
	var out []byte
	switch format {
	case ramdiskRaw:
		out = data
	case ramdiskGzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, 0, err
		}
		out, err = io.ReadAll(r)
		if err != nil {
			return nil, 0, err
		}
	case ramdiskXZ:
		r, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, 0, err
		}
		out, err = io.ReadAll(r)
		if err != nil {
			return nil, 0, err
		}
	case ramdiskLZ4Frame:
		out, err = io.ReadAll(lz4.NewReader(bytes.NewReader(data)))
		if err != nil {
			return nil, 0, err
		}
	case ramdiskLZ4Legacy:
		// Legacy frames: magic, then blocks of u32le size + LZ4 block
		// (size high bit set means stored uncompressed). Real Android
		// ramdisks are multiple frames concatenated (one per fragment),
		// which the kernel initramfs decompressor consumes in sequence.
		rest := data
		var buf bytes.Buffer
		block := make([]byte, 8<<20)
		for len(rest) > 0 {
			if !bytes.HasPrefix(rest, lz4LegacyMagic) {
				return nil, 0, fmt.Errorf("android: lz4 legacy frame out of sync at %d", len(data)-len(rest))
			}
			rest = rest[len(lz4LegacyMagic):]
			for len(rest) >= 4 && !bytes.HasPrefix(rest, lz4LegacyMagic) {
				size := binary.LittleEndian.Uint32(rest[:4])
				rest = rest[4:]
				if size == 0 {
					break
				}
				stored := size&0x80000000 != 0
				size &^= 0x80000000
				if int(size) > len(rest) {
					return nil, 0, fmt.Errorf("android: truncated lz4 legacy block")
				}
				src := rest[:size]
				rest = rest[size:]
				if stored {
					buf.Write(src)
					continue
				}
				n, err := lz4.UncompressBlock(src, block)
				if err != nil {
					return nil, 0, fmt.Errorf("android: lz4 block: %w", err)
				}
				buf.Write(block[:n])
			}
		}
		out = buf.Bytes()
	}
	if !bytes.HasPrefix(out, []byte("07070")) {
		return nil, 0, fmt.Errorf("android: decompressed ramdisk is not a newc cpio archive")
	}
	return out, format, nil
}

func compressRamdisk(cpio []byte, format ramdiskFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case ramdiskRaw:
		return cpio, nil
	case ramdiskGzip:
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(cpio); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case ramdiskXZ:
		w, err := xz.NewWriter(&buf)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(cpio); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case ramdiskLZ4Frame:
		w := lz4.NewWriter(&buf)
		if _, err := w.Write(cpio); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case ramdiskLZ4Legacy:
		buf.Write(lz4LegacyMagic)
		dst := make([]byte, lz4.CompressBlockBound(8<<20))
		for len(cpio) > 0 {
			chunk := cpio
			if len(chunk) > 8<<20 {
				chunk = chunk[:8<<20]
			}
			cpio = cpio[len(chunk):]
			n, err := lz4.CompressBlock(chunk, dst, nil)
			var hdr [4]byte
			if err != nil || n == 0 || n >= len(chunk) {
				// Stored block (high bit set): always valid.
				binary.LittleEndian.PutUint32(hdr[:], uint32(len(chunk))|0x80000000)
				buf.Write(hdr[:])
				buf.Write(chunk)
			} else {
				binary.LittleEndian.PutUint32(hdr[:], uint32(n))
				buf.Write(hdr[:])
				buf.Write(dst[:n])
			}
		}
	default:
		return nil, fmt.Errorf("android: no compressor for ramdisk format %d", format)
	}
	return buf.Bytes(), nil
}

// --- newc cpio ---

type cpioEntry struct {
	name                 string
	mode                 uint32
	uid                  uint32
	gid                  uint32
	data                 []byte
	ino                  uint32
	nlink                uint32
	mtime                uint32
	devMajor, devMinor   uint32
	rdevMajor, rdevMinor uint32
}

func parseNewc(data []byte) ([]cpioEntry, error) {
	var out []cpioEntry
	pos := 0
	for {
		if pos+110 > len(data) {
			return nil, fmt.Errorf("android: truncated cpio header at %d", pos)
		}
		hdr := string(data[pos : pos+110])
		if hdr[:6] != "070701" && hdr[:6] != "070702" {
			return nil, fmt.Errorf("android: bad cpio magic %q at %d", hdr[:6], pos)
		}
		field := func(i int) uint32 {
			v, _ := strconv.ParseUint(hdr[6+i*8:6+(i+1)*8], 16, 32)
			return uint32(v)
		}
		nameSize := int(field(11))
		fileSize := int(field(6))
		nameStart := pos + 110
		if nameStart+nameSize > len(data) {
			return nil, fmt.Errorf("android: truncated cpio name at %d", pos)
		}
		name := strings.TrimRight(string(data[nameStart:nameStart+nameSize]), "\x00")
		dataStart := align4(nameStart + nameSize)
		if dataStart+fileSize > len(data) {
			return nil, fmt.Errorf("android: truncated cpio data for %q", name)
		}
		if name == "TRAILER!!!" {
			// Android's generic ramdisk is several cpio archives
			// concatenated (one per fragment/frame); the kernel applies
			// them in order. Skipping to the next magic instead of
			// stopping is load-bearing — later archives carry files
			// like the first-stage fstab.
			pos = dataStart + fileSize
			next := bytes.Index(data[pos:], []byte("07070"))
			if next < 0 {
				break
			}
			pos += next
			continue
		}
		out = append(out, cpioEntry{
			name: name, ino: field(0), mode: field(1), uid: field(2), gid: field(3),
			nlink: field(4), mtime: field(5),
			devMajor: field(7), devMinor: field(8), rdevMajor: field(9), rdevMinor: field(10),
			data: append([]byte(nil), data[dataStart:dataStart+fileSize]...),
		})
		pos = align4(dataStart + fileSize)
	}
	return out, nil
}

func align4(n int) int { return (n + 3) &^ 3 }

func writeNewc(entries []cpioEntry) []byte {
	var buf bytes.Buffer
	write := func(e cpioEntry) {
		nlink := e.nlink
		if nlink == 0 {
			nlink = 1
		}
		fields := [13]uint32{e.ino, e.mode, e.uid, e.gid, nlink, e.mtime, uint32(len(e.data)),
			e.devMajor, e.devMinor, e.rdevMajor, e.rdevMinor, uint32(len(e.name) + 1), 0}
		if e.mode == 0 {
			fields[1] = 0o100644
		}
		buf.WriteString("070701")
		for _, f := range fields {
			fmt.Fprintf(&buf, "%08x", f)
		}
		buf.WriteString(e.name)
		buf.WriteByte(0)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
		buf.Write(e.data)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	ino := uint32(1)
	for _, e := range entries {
		if e.ino == 0 {
			e.ino = ino
		}
		ino++
		write(e)
	}
	write(cpioEntry{name: "TRAILER!!!", ino: 0})
	for buf.Len()%512 != 0 {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// patchRamdiskCPIO applies boot_patch.sh's entry changes to a parsed
// archive, byte-for-byte the way magiskboot does (verified against
// magiskboot's own output on this archive). The fstab `patch` op is
// omitted on purpose: with KEEPVERITY/KEEPFORCEENCRYPT set (our
// config), Magisk leaves the stock fstab semantics intact.
func patchRamdiskCPIO(entries []cpioEntry, assets map[string][]byte, magiskXZ, initLDXZ, origInitXZ []byte) []cpioEntry {
	set := func(name string, mode uint32, data []byte) {
		for i := range entries {
			if entries[i].name == name {
				entries[i].mode = mode
				entries[i].data = data
				return
			}
		}
		entries = append(entries, cpioEntry{name: name, mode: mode, data: data})
	}
	set("init", 0o100750, assets["magiskinit"])
	set("overlay.d", 0o040750, nil)
	set("overlay.d/sbin", 0o040750, nil)
	set("overlay.d/sbin/magisk.xz", 0o100644, magiskXZ)
	set("overlay.d/sbin/init-ld.xz", 0o100644, initLDXZ)
	set(".backup", 0o040000, nil)
	// magiskboot `backup`: original init, xz-compressed, keeping its
	// mode; magiskinit looks for exactly this name in first stage.
	set(".backup/init.xz", 0o100750, origInitXZ)
	// .rmlist: NUL-separated new entries, for restore.
	rmlist := "overlay.d\x00overlay.d/sbin\x00overlay.d/sbin/init-ld.xz\x00overlay.d/sbin/magisk.xz\x00"
	set(".backup/.rmlist", 0o100000, []byte(rmlist))
	config := "KEEPVERITY=true\nKEEPFORCEENCRYPT=true\nRECOVERYMODE=false\nVENDORBOOT=false\n"
	set(".backup/.magisk", 0o100000, []byte(config))
	return entries
}

// verifyPatchedCPIO is the equivalent of `magiskboot cpio test` == 1:
// /init is magiskinit and the .magisk config marker exists.
func verifyPatchedCPIO(entries []cpioEntry, magiskinit []byte) error {
	var haveInit, haveCfg, haveMagisk bool
	for _, e := range entries {
		switch e.name {
		case "init":
			haveInit = bytes.Equal(e.data, magiskinit)
		case ".backup/.magisk":
			haveCfg = true
		case "overlay.d/sbin/magisk.xz":
			haveMagisk = len(e.data) > 0
		}
	}
	if !haveInit || !haveCfg || !haveMagisk {
		return fmt.Errorf("android: patched ramdisk failed verification (init=%v config=%v magisk.xz=%v)", haveInit, haveCfg, haveMagisk)
	}
	return nil
}
