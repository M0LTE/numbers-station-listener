package ubersdr

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
)

func gz(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(raw)
	w.Close()
	return b.Bytes()
}

func v2Header(flags byte, seq uint16, centre uint64) []byte {
	b := append([]byte("SPEC"), 2, flags)
	b = binary.LittleEndian.AppendUint16(b, seq)
	b = binary.LittleEndian.AppendUint64(b, 123)
	return binary.LittleEndian.AppendUint64(b, centre)
}

// The captured stream: gzip config, one full frame, deltas, a second config
// after a zoom (no full frame follows it), more deltas.
func TestSpectrumFixture(t *testing.T) {
	recs := readRecords(t, "spectrum-v2-binary8-7910k.rec")
	d := &specDecoder{}
	configs, rows := 0, 0
	afterZoomRows := 0
	var first []float32
	for i, r := range recs {
		if r.text {
			t.Fatalf("record %d: unexpected text frame", i)
		}
		if isGzip(r.data) {
			c, err := decodeControl(r.data)
			if err != nil || c.Type != "config" {
				t.Fatalf("record %d: %v %+v", i, err, c)
			}
			configs++
			d.config(c)
			continue
		}
		row, ok, err := d.frame(r.data)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !ok {
			continue
		}
		rows++
		if configs > 1 {
			afterZoomRows++
		}
		if len(row.Levels) != 1024 || row.BinHz != 10 {
			t.Fatalf("record %d: %d bins at %v Hz", i, len(row.Levels), row.BinHz)
		}
		if row.StartHz != 7910000-512*10 || row.CenterHz() != 7910000 {
			t.Fatalf("record %d: start %v centre %v", i, row.StartHz, row.CenterHz())
		}
		for k, v := range row.Levels {
			if v < -146 || v > -60 || math.IsNaN(float64(v)) {
				t.Fatalf("record %d bin %d: level %v", i, k, v)
			}
		}
		if first == nil {
			first = append([]float32(nil), row.Levels...)
		}
	}
	if configs != 2 || rows != 20 || afterZoomRows != 0 {
		t.Fatalf("configs %d rows %d after zoom %d", configs, rows, afterZoomRows)
	}
	if d.gaps != 0 {
		t.Fatalf("%d sequence gaps", d.gaps)
	}

	// The first row matches an independent decode of the full frame: dB =
	// (ref + code*step)/100 with raw bin i at ordered index (i+512) mod 1024.
	full := recs[1].data
	ref := float64(int16(binary.LittleEndian.Uint16(full[24:26])))
	step := float64(full[26])
	codes := full[27:]
	for i, c := range codes {
		want := float32((ref + float64(c)*step) / 100)
		if got := first[(i+512)%1024]; got != want {
			t.Fatalf("raw bin %d: got %v want %v", i, got, want)
		}
	}
}

func TestSpectrumV2MaskAndRotation(t *testing.T) {
	d := &specDecoder{}
	d.config(specControl{Type: "config", BinCount: 10, BinBandwidth: 100})
	// Full frame: ref -100 dB, step 1 dB, raw codes 0..9.
	full := v2Header(specV2Full, 1, 1_000_000)
	full = binary.LittleEndian.AppendUint16(full, 0xD8F0) // -10000 centi-dB
	full = append(full, 100, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	row, ok, err := d.frame(full)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// n=10, half=5: ordered = raw[5..9], raw[0..4].
	want := []float32{-95, -94, -93, -92, -91, -100, -99, -98, -97, -96}
	for i := range want {
		if row.Levels[i] != want[i] {
			t.Fatalf("levels %v, want %v", row.Levels, want)
		}
	}
	if row.StartHz != 1_000_000-500 || row.BinHz != 100 {
		t.Fatalf("geometry %v %v", row.StartHz, row.BinHz)
	}

	// Delta: change raw bins 0 and 9 to codes 50 and 60. Mask bytes cover
	// 10 bins: byte0 bit0, byte1 bit1.
	delta := append(v2Header(specV2Delta, 2, 1_000_000), 0x01, 0x02, 50, 60)
	row, ok, err = d.frame(delta)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row.Levels[5] != -50 || row.Levels[4] != -40 || row.Levels[0] != -95 {
		t.Fatalf("after delta %v", row.Levels)
	}

	// A delta whose length disagrees with its mask is refused.
	if _, _, err := d.frame(append(v2Header(specV2Delta, 3, 1_000_000), 0x01, 0x00)); err == nil {
		t.Fatal("short delta accepted")
	}

	// A zoom invalidates the accumulator until the next full frame.
	d.config(specControl{Type: "config", BinCount: 10, BinBandwidth: 200})
	if _, ok, _ := d.frame(append(v2Header(specV2Delta, 4, 1_000_000), 0x00, 0x00)); ok {
		t.Fatal("delta after zoom emitted a row")
	}
	full2 := append(v2Header(specV2Full, 5, 2_000_000), full[24:]...)
	row, ok, err = d.frame(full2)
	if err != nil || !ok || row.BinHz != 200 || row.StartHz != 2_000_000-1000 {
		t.Fatalf("full after zoom: %v %v %+v", ok, err, row)
	}
	if d.gaps != 0 {
		t.Fatalf("gaps %d", d.gaps)
	}
	d.frame(append(v2Header(specV2Delta, 9, 2_000_000), 0x00, 0x00))
	if d.gaps != 1 {
		t.Fatalf("sequence gap not counted: %d", d.gaps)
	}
}

func TestSpectrumNoConfigNoRow(t *testing.T) {
	d := &specDecoder{}
	full := binary.LittleEndian.AppendUint16(v2Header(specV2Full, 1, 5), 0)
	full = append(full, 50, 1, 2)
	if _, ok, err := d.frame(full); ok || err != nil {
		t.Fatalf("row without bin width: %v %v", ok, err)
	}
}

func TestSpectrumV1Binary8(t *testing.T) {
	d := &specDecoder{}
	d.config(specControl{BinCount: 4, BinBandwidth: 10})
	hdr := func(flags byte) []byte {
		b := append([]byte("SPEC"), 1, flags)
		b = binary.LittleEndian.AppendUint64(b, 1)
		return binary.LittleEndian.AppendUint64(b, 1000)
	}
	row, ok, err := d.frame(append(hdr(specV1FullU8), 156, 157, 158, 159))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row.Levels[0] != -98 || row.Levels[2] != -100 {
		t.Fatalf("v1 levels %v", row.Levels)
	}
	delta := binary.LittleEndian.AppendUint16(hdr(specV1DeltaU8), 1)
	delta = binary.LittleEndian.AppendUint16(delta, 3)
	delta = append(delta, 200)
	row, ok, err = d.frame(delta)
	if err != nil || !ok || row.Levels[1] != -56 {
		t.Fatalf("v1 delta %v %v %v", ok, err, row.Levels)
	}
	if _, _, err := d.frame(append(hdr(0x01), 0, 0, 0, 0)); err == nil {
		t.Fatal("float32 frame accepted")
	}
	if _, _, err := d.frame([]byte("nope")); err == nil {
		t.Fatal("non-SPEC accepted")
	}
}

func TestDecodeControlError(t *testing.T) {
	c, err := decodeControl(gz(t, map[string]any{"type": "error", "error": "Failed to create spectrum session: maximum unique users reached (20)"}))
	if err != nil || c.Type != "error" || c.Error == "" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := decodeControl([]byte{0x1f, 0x8b, 0}); err == nil {
		t.Fatal("garbage gzip accepted")
	}
}

func TestBinWidthFor(t *testing.T) {
	cases := []struct {
		span, bins int
		want       float64
	}{
		{12000, 1024, 20}, {10240, 1024, 10}, {10000, 1024, 10}, {20480, 1024, 20},
		{12000, 2048, 10}, {12000, 512, 50}, {0, 0, 20}, {100_000_000, 1024, 5000},
	}
	for _, c := range cases {
		if got := binWidthFor(c.span, c.bins); got != c.want {
			t.Errorf("binWidthFor(%d, %d) = %v, want %v", c.span, c.bins, got, c.want)
		}
	}
}
