package androidemu

import (
	"bytes"
	"testing"
)

// Host-side Magisk ramdisk surgery: synthetic stock ramdisk in every
// framing we support must round-trip and come out verifiably patched —
// and never byte-identical to stock (the silent failure mode this code
// replaced).
func TestPatchRamdiskRoundTrips(t *testing.T) {
	stock := []cpioEntry{
		{name: "init", mode: 0o100750, data: []byte("STOCK-INIT"), ino: 1},
		{name: "etc", mode: 0o040755, ino: 2},
		{name: "etc/fstab", mode: 0o100644, data: []byte("fstab"), ino: 3},
	}
	assets := map[string][]byte{"magiskinit": []byte("MAGISKINIT"), "magisk": []byte("M"), "init-ld": []byte("L")}
	for _, format := range []ramdiskFormat{ramdiskRaw, ramdiskGzip, ramdiskXZ, ramdiskLZ4Legacy, ramdiskLZ4Frame} {
		stockBytes, err := compressRamdisk(writeNewc(stock), format)
		if err != nil {
			t.Fatalf("format %d compress stock: %v", format, err)
		}
		cpioData, gotFormat, err := decompressRamdisk(stockBytes)
		if err != nil {
			t.Fatalf("format %d decompress: %v", format, err)
		}
		if gotFormat != format {
			t.Fatalf("format %d sniffed as %d", format, gotFormat)
		}
		entries, err := parseNewc(cpioData)
		if err != nil {
			t.Fatalf("format %d parse: %v", format, err)
		}
		patched := patchRamdiskCPIO(entries, assets, []byte("magisk-xz"), []byte("initld-xz"), []byte("originit-xz"))
		if err := verifyPatchedCPIO(patched, assets["magiskinit"]); err != nil {
			t.Fatalf("format %d: %v", format, err)
		}
		out, err := compressRamdisk(writeNewc(patched), format)
		if err != nil {
			t.Fatalf("format %d compress patched: %v", format, err)
		}
		if bytes.Equal(out, stockBytes) {
			t.Fatalf("format %d: patched output identical to stock", format)
		}
		// Full re-parse of the shipped bytes.
		back, _, err := decompressRamdisk(out)
		if err != nil {
			t.Fatalf("format %d re-decompress: %v", format, err)
		}
		backEntries, err := parseNewc(back)
		if err != nil {
			t.Fatalf("format %d re-parse: %v", format, err)
		}
		if err := verifyPatchedCPIO(backEntries, assets["magiskinit"]); err != nil {
			t.Fatalf("format %d re-verify: %v", format, err)
		}
		// Stock files survive, original init backed up.
		names := map[string][]byte{}
		for _, e := range backEntries {
			names[e.name] = e.data
		}
		if string(names["etc/fstab"]) != "fstab" {
			t.Fatalf("format %d: stock file lost", format)
		}
		if string(names[".backup/init.xz"]) != "originit-xz" {
			t.Fatalf("format %d: original init not backed up", format)
		}
	}
}
