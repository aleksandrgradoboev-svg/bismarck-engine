package coding

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// A minimal valid 1x1 PNG (IHDR + IDAT + IEND).
func minimalPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // signature
		0x00, 0x00, 0x00, 0x0d, // IHDR length = 13
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1x1
		0x08, 0x06, 0x00, 0x00, 0x00, // bit depth/color/etc
		0x1f, 0x15, 0xc4, 0x89, // CRC
		0x00, 0x00, 0x00, 0x0a, // IDAT length
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, // data
		0x0d, 0x0a, 0x2d, 0xb4, // CRC
		0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82, // IEND
	}
}

// Animated PNG: IHDR (13) then an acTL chunk before IDAT.
func animatedPNG() []byte {
	out := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, // IHDR len 13
		0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00,
		0x1f, 0x15, 0xc4, 0x89,
		0x00, 0x00, 0x00, 0x08, // acTL len 8
		0x61, 0x63, 0x54, 0x4c, // "acTL"
		0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00,
		0xde, 0xad, 0xbe, 0xef, // CRC
	}
	return out
}

// tinyBMP1x1Red24bpp builds a minimal valid 1×1 24bpp BMP (BITMAPINFOHEADER),
// mirroring pi's createTinyBmp1x1Red24bpp test fixture.
func tinyBMP1x1Red24bpp() []byte {
	buf := make([]byte, 58)
	copy(buf[0:], "BM")
	putUint32LE(buf, 2, uint32(len(buf))) // declared file size
	putUint32LE(buf, 10, 54)              // pixel data offset
	putUint32LE(buf, 14, 40)              // DIB header size (BITMAPINFOHEADER)
	putUint32LE(buf, 18, 1)               // width
	putUint32LE(buf, 22, 1)               // height
	putUint16LE(buf, 26, 1)               // color planes
	putUint16LE(buf, 28, 24)              // bits per pixel
	putUint32LE(buf, 30, 0)               // compression (BI_RGB)
	putUint32LE(buf, 34, 4)               // image size
	buf[56] = 0xff                        // blue channel of the single pixel
	return buf
}

func putUint16LE(b []byte, off int, v uint16) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
}

func putUint32LE(b []byte, off int, v uint32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}

func TestDetectMimeBMP(t *testing.T) {
	if got := detectSupportedImageMimeType(tinyBMP1x1Red24bpp()); got != "image/bmp" {
		t.Fatalf("expected image/bmp, got %q", got)
	}
}

func TestDetectMimeInvalidBMPRejected(t *testing.T) {
	// Valid "BM" magic but an invalid bitsPerPixel (e.g. 7) must be rejected.
	buf := tinyBMP1x1Red24bpp()
	putUint16LE(buf, 28, 7)
	if got := detectSupportedImageMimeType(buf); got != "" {
		t.Fatalf("BMP with invalid bitsPerPixel should be rejected, got %q", got)
	}
	// colorPlanes != 1 must also be rejected.
	buf = tinyBMP1x1Red24bpp()
	putUint16LE(buf, 26, 2)
	if got := detectSupportedImageMimeType(buf); got != "" {
		t.Fatalf("BMP with colorPlanes!=1 should be rejected, got %q", got)
	}
}

// A BMP file on disk is read as an image attachment converted to PNG, with the
// conversion hint, mirroring pi's tools.test.ts BMP case.
func TestReadBMPConvertsToPNG(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "image.bmp")
	os.WriteFile(p, tinyBMP1x1Red24bpp(), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "image.bmp"})
	if err != nil {
		t.Fatal(err)
	}
	txt := resultText(r)
	if !strings.Contains(txt, "Read image file [image/png]") {
		t.Fatalf("expected PNG note, got: %q", txt)
	}
	if !strings.Contains(txt, "[Image converted from image/bmp to image/png.]") {
		t.Fatalf("expected conversion hint, got: %q", txt)
	}
	var img ai.ImageContent
	found := false
	for _, c := range r.Content {
		if ic, ok := c.(ai.ImageContent); ok {
			img = ic
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an image content block")
	}
	if img.MimeType != "image/png" {
		t.Fatalf("expected attachment mimeType image/png, got %q", img.MimeType)
	}
	raw, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("attachment data not base64: %v", err)
	}
	if len(raw) < 1 || raw[0] != 0x89 {
		t.Fatalf("attachment should be PNG (0x89 magic), got % x", raw[:min(len(raw), 4)])
	}
}

func TestDetectMimeRealPNG(t *testing.T) {
	if got := detectSupportedImageMimeType(minimalPNG()); got != "image/png" {
		t.Fatalf("expected image/png, got %q", got)
	}
}

func TestDetectMimeAnimatedPNGRejected(t *testing.T) {
	if got := detectSupportedImageMimeType(animatedPNG()); got != "" {
		t.Fatalf("animated PNG should be rejected, got %q", got)
	}
}

func TestDetectMimeNonIHDRPNGRejected(t *testing.T) {
	buf := minimalPNG()
	// Corrupt the IHDR chunk-type so it is no longer a valid PNG header.
	buf[12] = 'X'
	if got := detectSupportedImageMimeType(buf); got != "" {
		t.Fatalf("non-IHDR PNG should be rejected, got %q", got)
	}
}

func TestDetectMimeJPEG(t *testing.T) {
	if got := detectSupportedImageMimeType([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0}); got != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", got)
	}
}

func TestDetectMimeCMYKJPEGRejected(t *testing.T) {
	// ffd8fff7 = CMYK / extended sequential JPEG → rejected.
	if got := detectSupportedImageMimeType([]byte{0xff, 0xd8, 0xff, 0xf7, 0, 0}); got != "" {
		t.Fatalf("CMYK JPEG should be rejected, got %q", got)
	}
}

func TestDetectMimeGIFWebp(t *testing.T) {
	for _, sig := range []string{"GIF87a", "GIF89a"} {
		if got := detectSupportedImageMimeType([]byte(sig)); got != "image/gif" {
			t.Fatalf("%s: expected image/gif, got %q", sig, got)
		}
	}
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBP")...)
	if got := detectSupportedImageMimeType(webp); got != "image/webp" {
		t.Fatalf("expected image/webp, got %q", got)
	}
}

// An extensionless real PNG file reads as an image (detection by content).
func TestReadExtensionlessImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "screenshot") // no extension
	os.WriteFile(p, minimalPNG(), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "screenshot"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(r), "Read image file [image/png]") {
		t.Fatalf("extensionless PNG should read as image: %q", resultText(r))
	}
	hasImage := false
	for _, c := range r.Content {
		if _, ok := c.(ai.ImageContent); ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("expected an image content block")
	}
}

// A .png-named file with animated-PNG content falls back to the text path (no image).
func TestReadAnimatedPNGFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "anim.png")
	os.WriteFile(p, animatedPNG(), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "anim.png"})
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT be treated as an image: no "Read image file" note.
	if strings.Contains(resultText(r), "Read image file") {
		t.Fatalf("animated PNG should not be sent as image: %q", resultText(r))
	}
}

// A .png-named file with CMYK-JPEG content also falls back to text.
func TestReadCMYKMislabeledFallsBackToText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.png")
	os.WriteFile(p, []byte{0xff, 0xd8, 0xff, 0xf7, 0, 0, 0, 0}, 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "fake.png"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(r), "Read image file") {
		t.Fatalf("CMYK JPEG should not be sent as image: %q", resultText(r))
	}
}

// A text file that merely begins with "GIF" is text, not an image: pi requires
// the complete GIF87a/GIF89a signature (upstream 47a18e37b, issue #9755).
func TestDetectMimeGIFPrefixTextIsNotImage(t *testing.T) {
	for _, s := range []string{"GIF", "GIFs are fun\n", "GIF88a", "GIF89"} {
		if got := detectSupportedImageMimeType([]byte(s)); got != "" {
			t.Fatalf("%q: expected no image type, got %q", s, got)
		}
	}
}

// End to end: the read tool returns such a file as text.
func TestReadGIFPrefixedTextFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("GIF notes\nline two\n"), 0o644)
	r, err := run(t, readTool(dir), map[string]any{"path": "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(r); !strings.Contains(got, "GIF notes") || strings.Contains(got, "Read image file") {
		t.Fatalf("GIF-prefixed text should read as text: %q", got)
	}
}
