package coding

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

func TestResizeDownscalesOversizedImage(t *testing.T) {
	// 3000x2400 PNG exceeds the 2000px cap → must be downscaled.
	img := image.NewRGBA(image.Rect(0, 0, 3000, 2400))
	for y := 0; y < 2400; y++ {
		for x := 0; x < 3000; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, mime, ok := resizeImageForModel(buf.Bytes(), "image/png", nil)
	if !ok {
		t.Fatal("expected resize to succeed")
	}
	dec, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	b := dec.Bounds()
	if b.Dx() > imgMaxWidth || b.Dy() > imgMaxHeight {
		t.Fatalf("not downscaled: %dx%d", b.Dx(), b.Dy())
	}
	// aspect ratio preserved (3000:2400 = 5:4)
	if r := float64(b.Dx()) / float64(b.Dy()); r < 1.2 || r > 1.3 {
		t.Fatalf("aspect ratio not preserved: %dx%d", b.Dx(), b.Dy())
	}
	_ = mime
}

func TestSmallImagePassesThroughUnchanged(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	in := buf.Bytes()
	out, mime, ok := resizeImageForModel(in, "image/png", nil)
	if !ok || mime != "image/png" {
		t.Fatalf("small image should pass through: ok=%v mime=%s", ok, mime)
	}
	if !bytes.Equal(out, in) {
		t.Fatal("small image should be returned unchanged")
	}
}

func TestExifOrientationApplied(t *testing.T) {
	// Stored 2400x1200 (landscape, oversized → forces the resize path where pi
	// bakes orientation), dark, with a BRIGHT square in the top-left. EXIF
	// orientation 6 means "rotate 90° CW to display": the top-left must end up
	// TOP-RIGHT. This verifies the rotation *direction*, not just the dimension
	// swap (a wrong-way rotation would also swap dims but place the marker wrong).
	const sw, sh = 2400, 1200
	img := image.NewRGBA(image.Rect(0, 0, sw, sh))
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			c := color.RGBA{R: 10, G: 10, B: 10, A: 255}
			if x < sw/4 && y < sh/3 { // top-left marker
				c = color.RGBA{R: 250, G: 250, B: 250, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	withExif := injectExifOrientation(jbuf.Bytes(), 6)

	if o := jpegOrientation(withExif); o != 6 {
		t.Fatalf("orientation not parsed: got %d", o)
	}
	resized, ok := resizeImage(withExif, "image/jpeg", nil)
	if !ok {
		t.Fatal("resize failed")
	}
	if !resized.WasResized {
		t.Fatal("expected oversized oriented image to be resized (orientation baked)")
	}
	dec, _, _ := image.Decode(bytes.NewReader(resized.Data))
	b := dec.Bounds()
	if b.Dx() >= b.Dy() {
		t.Fatalf("orientation 6 should produce a portrait image, got %dx%d", b.Dx(), b.Dy())
	}
	bright := func(x, y int) uint32 { r, _, _, _ := dec.At(b.Min.X+x, b.Min.Y+y).RGBA(); return r >> 8 }
	topRight := bright(b.Dx()-b.Dx()/8, b.Dy()/12) // where the marker MUST be after a CW rotation
	topLeft := bright(b.Dx()/8, b.Dy()/12)         // where it must NOT be
	if topRight < 180 {
		t.Fatalf("marker not in top-right after orientation 6 (got brightness %d) — wrong rotation direction", topRight)
	}
	if topLeft > 120 {
		t.Fatalf("marker leaked into top-left (brightness %d) — wrong rotation", topLeft)
	}
}

// TestSmallOrientedImagePassesThrough verifies pi's contract: a small oriented
// JPEG is returned unchanged (EXIF intact, not baked), with post-orientation
// dimensions reported and wasResized=false.
func TestSmallOrientedImagePassesThrough(t *testing.T) {
	withExif := injectExifOrientation(jpegBytes(t, 40, 20), 6)

	r, ok := resizeImage(withExif, "image/jpeg", nil)
	if !ok {
		t.Fatal("resize failed")
	}
	if r.WasResized {
		t.Fatal("small image should not be resized")
	}
	if !bytes.Equal(r.Data, withExif) {
		t.Fatal("small image should be returned with original bytes unchanged (pi contract)")
	}
	// Orientation 6 swaps dimensions: stored 40x20 → reported 20x40.
	if r.Width != 20 || r.Height != 40 {
		t.Fatalf("expected post-orientation dims 20x40, got %dx%d", r.Width, r.Height)
	}
}

// TestOrientationAfterNonExifAPP1 is the Go counterpart of pi's "should apply
// EXIF orientation after an XMP APP1 segment" (upstream c6b00676b): a JPEG whose
// first APP1 is XMP and whose second APP1 carries the real Exif block must still
// pick up the orientation, both at the parser and through resizeImage.
func TestOrientationAfterNonExifAPP1(t *testing.T) {
	data := injectJpegSegments(jpegBytes(t, 40, 20),
		app1Segment([]byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta xmlns:x=\"adobe:ns:meta/\"/>")),
		app1Segment(exifPayload(exifTIFF(6))),
	)

	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("orientation after a non-Exif APP1: got %d, want 6", o)
	}

	r, ok := resizeImage(data, "image/jpeg", nil)
	if !ok {
		t.Fatal("resize failed")
	}
	// Orientation 6 swaps dimensions: stored 40x20 → reported 20x40.
	if r.Width != 20 || r.Height != 40 {
		t.Fatalf("expected post-orientation dims 20x40, got %dx%d", r.Width, r.Height)
	}
}

// TestOrientationSkipsFillBytes covers pi's `if (marker === 0xff) { offset++;
// continue; }` branch: JPEG markers may be preceded by any number of 0xFF fill
// bytes, and skipping them must not abandon the scan. The odd counts matter —
// with an even run of fill bytes a two-byte skip would still land on the real
// marker, so only an odd count distinguishes pi's one-byte skip from any other.
func TestOrientationSkipsFillBytes(t *testing.T) {
	for _, fill := range []int{1, 2, 3} {
		data := injectJpegSegments(jpegBytes(t, 40, 20),
			bytes.Repeat([]byte{0xFF}, fill), // fill bytes ahead of the APP1 marker
			app1Segment(exifPayload(exifTIFF(6))),
		)
		if o := jpegOrientation(data); o != 6 {
			t.Fatalf("orientation behind %d fill byte(s): got %d, want 6", fill, o)
		}
	}
}

// TestOrientationRequiresExifNulHeader pins pi's hasExifHeader, which compares
// all six bytes 45 78 69 66 00 00: an APP1 opening "Exif" but not "Exif\0\0" is
// NOT an Exif segment, so the scan must walk past it rather than reading the
// bytes six past its start as a TIFF header. Verified against a transcription of
// upstream getExifOrientation @ 64eeb82a4 on these exact bytes: pi returns 1.
func TestOrientationRequiresExifNulHeader(t *testing.T) {
	data := injectJpegSegments(jpegBytes(t, 40, 20),
		app1Segment(append([]byte("Exif\xFF\xFF"), exifTIFF(6)...)),
	)
	if o := jpegOrientation(data); o != 1 {
		t.Fatalf("APP1 without the two NUL bytes must not decide the scan: got %d, want 1", o)
	}
}

// TestOrientationSegmentLengthOverrun pins the order of pi's findJpegTiffOffset:
// the Exif header is tested BEFORE the segment length is even read, so an APP1
// whose declared length runs past the end of the file still decides the answer.
// Verified against upstream @ 64eeb82a4 on these exact bytes: pi returns 6.
func TestOrientationSegmentLengthOverrun(t *testing.T) {
	data := injectJpegSegments(jpegBytes(t, 40, 20),
		app1SegmentWithLength(exifPayload(exifTIFF(6)), 0xFFFF),
	)
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("APP1 with an overrunning declared length: got %d, want 6", o)
	}
}

// TestOrientationIFDOutsideDeclaredSegment pins that the TIFF walk is bounded by
// the whole buffer, not by the enclosing APP1: pi's findJpegTiffOffset returns
// only an offset and readOrientationFromTiff then walks `bytes`. Here the APP1
// declares just the "Exif\0\0" header plus the 8-byte TIFF header, and the IFD
// sits past the segment end. Verified against upstream @ 64eeb82a4: pi = 6.
func TestOrientationIFDOutsideDeclaredSegment(t *testing.T) {
	const declared = 2 + 6 + 8 // length field + "Exif\0\0" + TIFF header, IFD excluded
	data := injectJpegSegments(jpegBytes(t, 40, 20),
		app1SegmentWithLength(exifPayload(exifTIFF(6)), declared),
	)
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("IFD past the declared segment end: got %d, want 6", o)
	}
}

// TestOrientationByteOrderFieldOnlyChecksII pins pi's readOrientationFromTiff,
// which computes `le = byteOrder === 0x4949` and validates nothing else: "MM"
// and any other byte-order field alike are read big-endian, and the 0x2A magic
// is never checked. Verified against upstream @ 64eeb82a4 on these exact bytes:
// pi returns 6 for both.
func TestOrientationByteOrderFieldOnlyChecksII(t *testing.T) {
	for _, byteOrder := range []string{"MM", "XY"} {
		data := injectJpegSegments(jpegBytes(t, 40, 20),
			app1Segment(exifPayload(bigEndianTIFF(byteOrder, 6))),
		)
		if o := jpegOrientation(data); o != 6 {
			t.Fatalf("byte-order field %q must be read big-endian: got %d, want 6", byteOrder, o)
		}
	}
}

// TestOrientationIgnoresTIFFMagic pins the other half of readOrientationFromTiff's
// leniency: bytes 2-3 of the TIFF header (the 0x2A magic) are never read, so a
// block whose magic is wrong is still parsed. Verified against upstream
// @ 64eeb82a4 on these exact bytes: pi returns 6.
func TestOrientationIgnoresTIFFMagic(t *testing.T) {
	tiff := exifTIFF(6)
	tiff[2], tiff[3] = 0x00, 0x00 // clobber the 0x2A magic
	data := injectJpegSegments(jpegBytes(t, 40, 20), app1Segment(exifPayload(tiff)))
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("TIFF magic must not be checked: got %d, want 6", o)
	}
}

// TestOrientationEntryTruncatedAtEOF pins readOrientationFromTiff's per-entry
// bound (`entryPos + 12 > bytes.length` returns 1): an IFD entry that does not
// fit in the buffer is not read, even when its tag and value bytes happen to be
// present. Verified against upstream @ 64eeb82a4 on these exact bytes: pi
// returns 1.
func TestOrientationEntryTruncatedAtEOF(t *testing.T) {
	tiff := exifTIFF(6)
	// Drop the next-IFD offset and the entry's last two value bytes.
	tiff = tiff[:len(tiff)-6]
	data := injectJpegSegments([]byte{0xFF, 0xD8}, app1Segment(exifPayload(tiff))) // the segment ends the file
	if o := jpegOrientation(data); o != 1 {
		t.Fatalf("Orientation entry running past EOF must not be read: got %d, want 1", o)
	}
}

// TestOrientationScanContinuesPastSOSAndEOI pins that pi's marker walk knows
// nothing about SOS or EOI: it reads the two bytes after any marker as a segment
// length and keeps going, so an Exif APP1 sitting behind a start-of-scan (or a
// spuriously length-prefixed EOI) is still found. Verified against upstream
// @ 64eeb82a4 on these exact bytes: pi returns 6 for both.
func TestOrientationScanContinuesPastSOSAndEOI(t *testing.T) {
	scan := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55} // stand-in for entropy-coded data
	exif := app1Segment(exifPayload(exifTIFF(6)))
	tests := []struct {
		name string
		data []byte
	}{
		{"behind SOS", injectJpegSegments([]byte{0xFF, 0xD8},
			append([]byte{0xFF, 0xDA, 0x00, byte(len(scan) + 2)}, scan...), exif, []byte{0xFF, 0xD9})},
		{"behind EOI", injectJpegSegments([]byte{0xFF, 0xD8},
			[]byte{0xFF, 0xD9, 0x00, 0x02}, exif)},
	}
	for _, tt := range tests {
		if o := jpegOrientation(tt.data); o != 6 {
			t.Errorf("Exif APP1 %s: got %d, want 6", tt.name, o)
		}
	}
}

// TestWebpOrientation covers findWebpTiffOffset: the EXIF chunk is located by
// walking the RIFF chunks (odd-sized chunks are padded to even), and its payload
// may or may not carry the "Exif\0\0" prefix ahead of the TIFF header. A chunk
// whose declared size runs past the end of the file yields 1. Every case was
// verified against a transcription of upstream @ 64eeb82a4 on these exact bytes.
func TestWebpOrientation(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{"bare TIFF payload", webpContainer(riffChunk("EXIF", exifTIFF(6))), 6},
		{
			"Exif-prefixed payload after an odd-sized chunk",
			webpContainer(riffChunk("VP8 ", []byte{1, 2, 3}), riffChunk("EXIF", exifPayload(exifTIFF(6)))),
			6,
		},
		{"no EXIF chunk", webpContainer(riffChunk("VP8 ", []byte{1, 2, 3})), 1},
		// A chunk too small to hold the prefix is read as a bare TIFF block
		// starting at "Exif\0\0" itself, which yields no orientation.
		{"declared size below the Exif prefix", webpContainer(riffChunkWithSize("EXIF", exifPayload(exifTIFF(6)), 4)), 1},
		{"chunk size past end of file", webpContainer([]byte("EXIF\xFF\xFF\x00\x00")), 1},
		// pi reads the chunk size with JS bitwise ops, i.e. as a SIGNED 32-bit
		// value: a size with the top bit set is negative, passes the end-of-file
		// check and (being < 6) selects the bare-TIFF reading of the payload.
		{"chunk size with the sign bit set", webpContainer(riffChunkWithSize("EXIF", exifTIFF(6), 0xFFFFFFF8)), 6},
	}
	for _, tt := range tests {
		if o := webpOrientation(tt.data); o != tt.want {
			t.Errorf("webpOrientation(%s): got %d, want %d", tt.name, o, tt.want)
		}
		if o := exifOrientationFromBytes(tt.data); o != tt.want {
			t.Errorf("exifOrientationFromBytes(%s): got %d, want %d", tt.name, o, tt.want)
		}
	}
}

// TestFirstExifSegmentDecides pins pi's findJpegTiffOffset: the FIRST APP1
// carrying the "Exif\0\0" header wins outright — pi returns that segment's TIFF
// offset and reads orientation 1 out of it when there is no Orientation tag. It
// never falls through to a later Exif APP1.
func TestFirstExifSegmentDecides(t *testing.T) {
	data := injectJpegSegments(jpegBytes(t, 40, 20),
		app1Segment(exifPayload(exifTIFF(0))), // Exif APP1 with an empty IFD
		app1Segment(exifPayload(exifTIFF(6))),
	)

	if o := jpegOrientation(data); o != 1 {
		t.Fatalf("first Exif APP1 must decide: got %d, want 1", o)
	}
}

// TestReadToolResizesImage exercises the read tool end-to-end on an oversized image.
func TestReadToolResizesImage(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 2500, 100))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	os.WriteFile(filepath.Join(dir, "big.png"), buf.Bytes(), 0o644)

	r, err := readTool(dir).Execute(context.Background(), "id", map[string]any{"path": "big.png"}, func(agent.AgentToolResult) {})
	if err != nil {
		t.Fatal(err)
	}
	var imgContent *ai.ImageContent
	var text string
	for _, c := range r.Content {
		switch v := c.(type) {
		case ai.ImageContent:
			ic := v
			imgContent = &ic
		case ai.TextContent:
			text = v.Text
		}
	}
	if imgContent == nil {
		t.Fatalf("no image content returned; text=%q", text)
	}
	if imgContent.Data == "" {
		t.Fatal("empty image data")
	}
}

// exifTIFF builds a minimal little-endian TIFF block. A non-zero orientation
// gets a single Orientation (0x0112, SHORT) IFD entry; orientation 0 yields an
// empty IFD, i.e. a valid Exif block that carries no orientation.
func exifTIFF(orientation int) []byte {
	tiff := []byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00} // header, IFD at offset 8
	if orientation == 0 {
		tiff = append(tiff, 0x00, 0x00) // 0 entries
	} else {
		tiff = append(tiff, 0x01, 0x00) // 1 entry
		tiff = append(tiff,
			0x12, 0x01, // tag 0x0112
			0x03, 0x00, // type SHORT
			0x01, 0x00, 0x00, 0x00, // count 1
			byte(orientation), 0x00, 0x00, 0x00, // value
		)
	}
	return append(tiff, 0x00, 0x00, 0x00, 0x00) // next IFD offset = 0
}

// bigEndianTIFF builds a minimal big-endian TIFF block with a single
// Orientation entry, under an arbitrary byte-order field ("MM" for a
// well-formed one).
func bigEndianTIFF(byteOrder string, orientation int) []byte {
	tiff := []byte{byteOrder[0], byteOrder[1], 0x00, 0x2A, 0x00, 0x00, 0x00, 0x08} // header, IFD at offset 8
	tiff = append(tiff, 0x00, 0x01)                                                // 1 entry
	tiff = append(tiff,
		0x01, 0x12, // tag 0x0112
		0x00, 0x03, // type SHORT
		0x00, 0x00, 0x00, 0x01, // count 1
		0x00, byte(orientation), 0x00, 0x00, // value
	)
	return append(tiff, 0x00, 0x00, 0x00, 0x00) // next IFD offset = 0
}

// exifPayload prefixes a TIFF block with the "Exif\0\0" APP1 header.
func exifPayload(tiff []byte) []byte {
	return append([]byte("Exif\x00\x00"), tiff...)
}

// app1Segment wraps a payload in a JPEG APP1 (0xFFE1) segment.
func app1Segment(payload []byte) []byte {
	return app1SegmentWithLength(payload, len(payload)+2)
}

// app1SegmentWithLength wraps a payload in an APP1 segment whose declared length
// need not match the payload, so that the parser's use of the length can be
// pinned independently of the payload it precedes.
func app1SegmentWithLength(payload []byte, declared int) []byte {
	return append([]byte{0xFF, 0xE1, byte(declared >> 8), byte(declared)}, payload...)
}

// riffChunk builds a RIFF chunk (4-byte id, little-endian size, payload padded
// to an even length).
func riffChunk(id string, payload []byte) []byte {
	return riffChunkWithSize(id, payload, len(payload))
}

// riffChunkWithSize builds a RIFF chunk whose declared size need not match the
// payload that follows it.
func riffChunkWithSize(id string, payload []byte, declared int) []byte {
	header := append([]byte(id), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(header[4:8], uint32(declared))
	out := append(header, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// webpContainer wraps chunks in a RIFF/WEBP container.
func webpContainer(chunks ...[]byte) []byte {
	body := append([]byte("WEBP"), bytes.Join(chunks, nil)...)
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	return append(out, body...)
}

// jpegBytes encodes a blank w×h JPEG for the marker-walking tests.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// injectJpegSegments splices raw segment bytes in right after the JPEG SOI marker.
func injectJpegSegments(jpegData []byte, segments ...[]byte) []byte {
	out := append([]byte{0xFF, 0xD8}, bytes.Join(segments, nil)...)
	return append(out, jpegData[2:]...) // rest of original (after its SOI)
}

// injectExifOrientation inserts a minimal Exif APP1 segment with the given
// orientation right after the JPEG SOI marker.
func injectExifOrientation(jpegData []byte, orientation int) []byte {
	return injectJpegSegments(jpegData, app1Segment(exifPayload(exifTIFF(orientation))))
}

// TestDecodeNodeBase64MatchesNode locks Go's decoding to Node's
// Buffer.from(value, "base64"), which pi's image path relies on. Expectations
// were produced by running each input through Node 24 (Buffer.from(c,"base64")
// .toString("hex")). Go's StdEncoding rejects every lenient case below, which
// let a whitespace-wrapped or base64url payload through unresized.
func TestDecodeNodeBase64MatchesNode(t *testing.T) {
	cases := []struct{ in, wantHex string }{
		{"aGVsbG8=", "68656c6c6f"},     // canonical
		{"aGVs bG8=", "68656c6c6f"},    // embedded space
		{"\naGVsbG8=\n", "68656c6c6f"}, // wrapping newlines
		{"aGVsbG8", "68656c6c6f"},      // no padding
		{"aGVsbG8==", "68656c6c6f"},    // over-padded
		{"a-_GVsbG8", "6befc656c6c6"},  // base64url alphabet
		{"abc", "69b7"},                // len%4==3
		{"YQ", "61"},                   // len%4==2
	}
	for _, c := range cases {
		got := decodeNodeBase64(c.in)
		if hex.EncodeToString(got) != c.wantHex {
			t.Errorf("decodeNodeBase64(%q) = %s, want %s", c.in, hex.EncodeToString(got), c.wantHex)
		}
	}
}

// TestResizeHonorsModelProfile pins upstream f5c946480: a model's
// inputLimits.images.resize narrows the pipeline defaults, so an image that
// clears the 2000px cap is still downscaled when the model asks for less. The
// 0.86.1 catalog stamps the historical 2000/2000/4.5MiB/80 profile onto every
// vision model, so today every profile resolves to the old numbers — but a
// provider narrowing one limit has to reach the resize, which is what this
// pins. A nil profile keeps the defaults.
func TestResizeHonorsModelProfile(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 300; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 7, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()

	// Without a profile the image is already inside every default limit.
	if r, ok := resizeImage(src, "image/png", nil); !ok || r.WasResized || r.Width != 300 {
		t.Fatalf("nil profile must keep the defaults: %+v ok=%v", r, ok)
	}

	maxWidth := 100
	r, ok := resizeImage(src, "image/png", &ai.ModelImageResizeOptions{MaxWidth: &maxWidth})
	if !ok {
		t.Fatal("resize with a narrowed profile failed")
	}
	if !r.WasResized || r.Width != 100 || r.Height != 80 {
		t.Fatalf("maxWidth 100 should give 100x80 (aspect preserved), got %dx%d wasResized=%v",
			r.Width, r.Height, r.WasResized)
	}
	if r.OriginalWidth != 300 || r.OriginalHeight != 240 {
		t.Fatalf("original dimensions lost: %dx%d", r.OriginalWidth, r.OriginalHeight)
	}
}

// TestResolveResizeProfileDedupesQualitySteps pins pi's
// `Array.from(new Set([opts.jpegQuality, 85, 70, 55, 40]))`: the configured
// quality leads, and a duplicate later in the list drops out rather than being
// tried twice.
func TestResolveResizeProfileDedupesQualitySteps(t *testing.T) {
	if got := resolveResizeProfile(nil).jpegQualities; !equalInts(got, []int{80, 85, 70, 55, 40}) {
		t.Fatalf("default quality steps = %v, want [80 85 70 55 40]", got)
	}
	q := 70
	if got := resolveResizeProfile(&ai.ModelImageResizeOptions{JPEGQuality: &q}).jpegQualities; !equalInts(got, []int{70, 85, 55, 40}) {
		t.Fatalf("quality 70 steps = %v, want [70 85 55 40]", got)
	}
	q = 55
	if got := resolveResizeProfile(&ai.ModelImageResizeOptions{JPEGQuality: &q}).jpegQualities; !equalInts(got, []int{55, 85, 70, 40}) {
		t.Fatalf("quality 55 steps = %v, want [55 85 70 40]", got)
	}
}

// The 0.87.0 catalog is the first to carry f5c946480's inputLimits, stamping a
// resize profile on every image-capable model. That profile must resolve to the
// pipeline defaults exactly — the regen was licensed as changing no resize
// decision — and a model without image input must carry none. Derived from the
// catalog, with a precondition so a future regen cannot leave it vacuous. It is
// a REGEN TRIPWIRE, not a parity oracle: if a faithful regen goes red here, pi
// has started narrowing a profile — follow the catalog and re-check the resize
// decisions; never edit the catalog or relax this to make it pass silently.
func TestCatalogResizeProfilesMatchPipelineDefaults(t *testing.T) {
	want := resolveResizeProfile(nil)
	vision := 0
	for _, provider := range ai.GetProviders() {
		for _, m := range ai.GetModels(provider) {
			var resize *ai.ModelImageResizeOptions
			if m.InputLimits != nil && m.InputLimits.Images != nil {
				resize = m.InputLimits.Images.Resize
			}
			if !slices.Contains(m.Input, "image") {
				if m.InputLimits != nil {
					t.Errorf("%s/%s has no image input but carries inputLimits", provider, m.ID)
				}
				continue
			}
			vision++
			if resize == nil {
				t.Errorf("%s/%s takes images but carries no resize profile", provider, m.ID)
				continue
			}
			got := resolveResizeProfile(resize)
			if got.maxWidth != want.maxWidth || got.maxHeight != want.maxHeight || got.maxBytes != want.maxBytes ||
				!equalInts(got.jpegQualities, want.jpegQualities) {
				t.Errorf("%s/%s resize profile %+v, want the pipeline defaults %+v", provider, m.ID, got, want)
			}
		}
	}
	if vision == 0 {
		t.Fatal("catalog has no image-capable models; the check above is vacuous")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestToFixed2MatchesJS pins toFixed2 to Number.prototype.toFixed(2), whose
// output feeds the "Multiply coordinates by N" note the model reads. Every want
// was captured by running Node. The spec picks, of the two nearest candidates
// for an EXACT tie, the larger — round half UP — where strconv.FormatFloat
// rounds an exact tie to EVEN. So the odd eighths diverged: a 2250px image
// under the 2000px limit read 1.12 in the port and 1.13 in pi. Non-ties such as
// 1.005 (binary 1.00499…) must keep rounding by the exact binary value, which
// both sides already did; they are here so the fix cannot trade one bug for
// another. Found by this cycle's JS-semantics review; this cycle also started
// writing the note into the user's own prompt text, where it is recorded and
// re-sent on every later request.
func TestToFixed2MatchesJS(t *testing.T) {
	cases := []struct {
		x    float64
		want string
	}{
		{1.125, "1.13"}, {1.625, "1.63"}, {2.125, "2.13"}, {0.125, "0.13"},
		{1.375, "1.38"}, {1.875, "1.88"},
		{1.005, "1.00"}, {1.015, "1.01"}, {1.025, "1.02"}, {2.675, "2.67"},
		{1.1, "1.10"}, {1.0, "1.00"}, {1.5, "1.50"}, {1234.5625, "1234.56"},
		{0.005, "0.01"}, {9.995, "9.99"}, {1e-7, "0.00"}, {3.14159, "3.14"},
		{2250.0 / 2000, "1.13"}, {2345.0 / 2000, "1.17"},
	}
	for _, c := range cases {
		if got := toFixed2(c.x); got != c.want {
			t.Errorf("toFixed2(%v) = %q, JS toFixed(2) gives %q", c.x, got, c.want)
		}
	}
}

// TestDimensionNoteMatchesPiAtTies pins the whole model-visible note at the
// ties toFixed2 used to round the other way. Both strings were produced by the
// 0.86.1 build's formatDimensionNote.
func TestDimensionNoteMatchesPiAtTies(t *testing.T) {
	cases := []struct {
		r    ResizeResult
		want string
	}{
		{ResizeResult{OriginalWidth: 2250, OriginalHeight: 1000, Width: 2000, Height: 889, WasResized: true},
			"[Image: original 2250x1000, displayed at 2000x889. Multiply coordinates by 1.13 to map to original image.]"},
		{ResizeResult{OriginalWidth: 3250, OriginalHeight: 1000, Width: 2000, Height: 615, WasResized: true},
			"[Image: original 3250x1000, displayed at 2000x615. Multiply coordinates by 1.63 to map to original image.]"},
	}
	for _, c := range cases {
		if got := formatDimensionNote(c.r); got != c.want {
			t.Errorf("formatDimensionNote = %q\n want pi's %q", got, c.want)
		}
	}
}
