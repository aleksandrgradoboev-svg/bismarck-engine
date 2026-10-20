// Package imagemime identifies the image formats tools attach and reads the
// size an image declares, without decoding it. The coding tools and the
// durable tools share it: pi's coding agent (utils/mime.ts) and pi-durable
// (tools/image.ts) detect formats the same way.
package imagemime

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

const (
	// headerBytes is what every check except the APNG chunk walk needs: BMP
	// reads up to offset 29.
	headerBytes = 32
	blockBytes  = 64 * 1024
)

// Detect sniffs magic bytes to identify a supported image type, or "": CMYK
// JPEG (ffd8fff7), animated PNG (acTL), PNG without IHDR, and an invalid BMP
// are not supported.
func Detect(buf []byte) string {
	if bytesStartWith(buf, []byte{0xff, 0xd8, 0xff}) {
		if len(buf) > 3 && buf[3] == 0xf7 {
			return ""
		}
		return "image/jpeg"
	}
	if bytesStartWith(buf, pngSignature) {
		if isPNG(buf) && !isAnimatedPNG(buf) {
			return "image/png"
		}
		return ""
	}
	if startsWithASCII(buf, 0, "GIF87a") || startsWithASCII(buf, 0, "GIF89a") {
		return "image/gif"
	}
	if startsWithASCII(buf, 0, "RIFF") && startsWithASCII(buf, 8, "WEBP") {
		return "image/webp"
	}
	if startsWithASCII(buf, 0, "BM") && isBmp(buf) {
		return "image/bmp"
	}
	return ""
}

// ReadAt reads up to length bytes at offset of a file; fewer only at its end.
type ReadAt func(offset, length int64) ([]byte, error)

// DetectOf is Detect of a whole file of size bytes (pi
// detectSupportedImageMimeTypeOf), reading only its header and, for PNG, the
// chunk headers up to the first acTL or IDAT.
func DetectOf(size int64, read ReadAt) (string, error) {
	header, err := read(0, headerBytes)
	if err != nil {
		return "", err
	}
	if !bytesStartWith(header, pngSignature) {
		return Detect(header), nil
	}
	if !isPNG(header) {
		return "", nil
	}
	animated, err := isAnimatedPNGOf(size, read)
	if err != nil || animated {
		return "", err
	}
	return "image/png", nil
}

// isAnimatedPNGOf is isAnimatedPNG over a file read in blocks.
func isAnimatedPNGOf(size int64, read ReadAt) (bool, error) {
	var block []byte
	var blockStart int64
	bytesAt := func(offset, length int64) ([]byte, error) {
		if offset < blockStart || offset+length > blockStart+int64(len(block)) {
			b, err := read(offset, blockBytes)
			if err != nil {
				return nil, err
			}
			blockStart, block = offset, b
		}
		from := offset - blockStart
		to := min(from+length, int64(len(block)))
		return block[from:to], nil
	}
	offset := int64(len(pngSignature))
	for offset+8 <= size {
		chunkHeader, err := bytesAt(offset, 8)
		if err != nil {
			return false, err
		}
		chunkLength := int64(readUint32BE(chunkHeader, 0))
		if startsWithASCII(chunkHeader, 4, "acTL") {
			return true, nil
		}
		if startsWithASCII(chunkHeader, 4, "IDAT") {
			return false, nil
		}
		next := offset + 8 + chunkLength + 4
		if next <= offset || next > size {
			return false, nil
		}
		offset = next
	}
	return false, nil
}

// Dimensions is the width and height an image of mimeType declares in its
// header (pi imageDimensions): PNG's IHDR, GIF's screen, WebP's VP8, VP8L or
// VP8X chunk, JPEG's first SOF segment. ok is false when the header cannot be
// read.
func Dimensions(b []byte, mimeType string) (width, height int, ok bool) {
	switch mimeType {
	case "image/png":
		if len(b) < 24 {
			return 0, 0, false
		}
		return readUint32BE(b, 16), readUint32BE(b, 20), true
	case "image/gif":
		if len(b) < 10 {
			return 0, 0, false
		}
		return readUint16LE(b, 6), readUint16LE(b, 8), true
	case "image/webp":
		if len(b) < 30 {
			return 0, 0, false
		}
		switch {
		case startsWithASCII(b, 12, "VP8 "):
			return readUint16LE(b, 26) & 0x3fff, readUint16LE(b, 28) & 0x3fff, true
		case startsWithASCII(b, 12, "VP8L"):
			bits := readUint32LE(b, 21)
			return bits&0x3fff + 1, (bits>>14)&0x3fff + 1, true
		case startsWithASCII(b, 12, "VP8X"):
			uint24 := func(offset int) int { return readUint16LE(b, offset) + byteAt(b, offset+2)<<16 }
			return uint24(24) + 1, uint24(27) + 1, true
		}
		return 0, 0, false
	case "image/jpeg":
		for offset := 2; offset+9 <= len(b); {
			if b[offset] != 0xff {
				return 0, 0, false
			}
			marker := b[offset+1]
			if marker == 0xff {
				offset++
				continue
			}
			// SOF0 to SOF15, except DHT (C4), JPG (C8), and DAC (CC), which
			// share the range.
			if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
				return readUint16BE(b, offset+7), readUint16BE(b, offset+5), true
			}
			offset += 2 + readUint16BE(b, offset+2)
		}
		return 0, 0, false
	}
	return 0, 0, false
}

// isBmp validates the BMP magic and DIB header: one color plane, a standard
// bit depth, and a consistent declared size and pixel data offset.
func isBmp(buf []byte) bool {
	if len(buf) < 26 {
		return false
	}
	declaredFileSize := readUint32LE(buf, 2)
	pixelDataOffset := readUint32LE(buf, 10)
	dibHeaderSize := readUint32LE(buf, 14)
	if declaredFileSize != 0 && declaredFileSize < 26 {
		return false
	}
	if pixelDataOffset < 14+dibHeaderSize {
		return false
	}
	if declaredFileSize != 0 && pixelDataOffset >= declaredFileSize {
		return false
	}

	var colorPlanes, bitsPerPixel int
	if dibHeaderSize == 12 {
		colorPlanes = readUint16LE(buf, 22)
		bitsPerPixel = readUint16LE(buf, 24)
	} else if dibHeaderSize >= 40 && dibHeaderSize <= 124 {
		if len(buf) < 30 {
			return false
		}
		colorPlanes = readUint16LE(buf, 26)
		bitsPerPixel = readUint16LE(buf, 28)
	} else {
		return false
	}
	if colorPlanes != 1 {
		return false
	}
	switch bitsPerPixel {
	case 1, 4, 8, 16, 24, 32:
		return true
	}
	return false
}

func isPNG(buf []byte) bool {
	return len(buf) >= 16 && readUint32BE(buf, len(pngSignature)) == 13 && startsWithASCII(buf, 12, "IHDR")
}

func isAnimatedPNG(buf []byte) bool {
	offset := len(pngSignature)
	for offset+8 <= len(buf) {
		chunkLength := readUint32BE(buf, offset)
		chunkTypeOffset := offset + 4
		if startsWithASCII(buf, chunkTypeOffset, "acTL") {
			return true
		}
		if startsWithASCII(buf, chunkTypeOffset, "IDAT") {
			return false
		}
		nextOffset := offset + 8 + chunkLength + 4
		if nextOffset <= offset || nextOffset > len(buf) {
			return false
		}
		offset = nextOffset
	}
	return false
}

// byteAt is buf[i], or 0 past the end, as JS's `buffer[i] ?? 0`.
func byteAt(buf []byte, i int) int {
	if i >= 0 && i < len(buf) {
		return int(buf[i])
	}
	return 0
}

func readUint16LE(buf []byte, offset int) int {
	return byteAt(buf, offset) + byteAt(buf, offset+1)<<8
}

func readUint16BE(buf []byte, offset int) int {
	return byteAt(buf, offset)<<8 + byteAt(buf, offset+1)
}

func readUint32LE(buf []byte, offset int) int {
	return byteAt(buf, offset) + byteAt(buf, offset+1)<<8 + byteAt(buf, offset+2)<<16 + byteAt(buf, offset+3)*0x1000000
}

func readUint32BE(buf []byte, offset int) int {
	return byteAt(buf, offset)*0x1000000 + byteAt(buf, offset+1)<<16 + byteAt(buf, offset+2)<<8 + byteAt(buf, offset+3)
}

func bytesStartWith(buf, prefix []byte) bool {
	if len(buf) < len(prefix) {
		return false
	}
	for i := range prefix {
		if buf[i] != prefix[i] {
			return false
		}
	}
	return true
}

func startsWithASCII(buf []byte, offset int, text string) bool {
	if len(buf) < offset+len(text) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if buf[offset+i] != text[i] {
			return false
		}
	}
	return true
}
