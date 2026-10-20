package images

// EXIF orientation of JPEG and WebP files, read from the bytes without
// decoding the image (pi durable/src/images/exif.ts). It is durable's own
// reader: unlike the coding pipeline's, it reads sizes unsigned and takes a
// WebP EXIF chunk without checking its declared size.

// byteAt is b[i], or 0 past either end, as JS bitwise operators read an
// absent index.
func byteAt(b []byte, i int64) int64 {
	if i < 0 || i >= int64(len(b)) {
		return 0
	}
	return int64(b[i])
}

// ExifOrientation is the EXIF orientation of a JPEG or WebP, 1 to 8; 1 when
// absent or unreadable.
func ExifOrientation(b []byte) int {
	n := int64(len(b))
	tiff := tiffStart(b)
	if tiff < 0 || tiff+8 > n {
		return 1
	}
	little := b[tiff] == 0x49 && b[tiff+1] == 0x49
	u16 := func(at int64) int64 {
		if little {
			return byteAt(b, at) | byteAt(b, at+1)<<8
		}
		return byteAt(b, at)<<8 | byteAt(b, at+1)
	}
	u32 := func(at int64) int64 {
		if little {
			return byteAt(b, at) | byteAt(b, at+1)<<8 | byteAt(b, at+2)<<16 | byteAt(b, at+3)<<24
		}
		return byteAt(b, at)<<24 | byteAt(b, at+1)<<16 | byteAt(b, at+2)<<8 | byteAt(b, at+3)
	}
	directory := tiff + u32(tiff+4)
	if directory+2 > n {
		return 1
	}
	entries := u16(directory)
	for index := int64(0); index < entries; index++ {
		entry := directory + 2 + index*12
		if entry+12 > n {
			return 1
		}
		// Tag 0x0112 is Orientation, a SHORT whose value sits in the entry.
		if u16(entry) == 0x0112 {
			if value := u16(entry + 8); value >= 1 && value <= 8 {
				return int(value)
			}
			return 1
		}
	}
	return 1
}

// tiffStart is where the TIFF header of a JPEG's APP1 or a WebP's EXIF chunk
// starts; -1 when there is none.
func tiffStart(b []byte) int64 {
	n := int64(len(b))
	if n >= 2 && b[0] == 0xff && b[1] == 0xd8 {
		for offset := int64(2); offset+4 <= n; {
			if b[offset] != 0xff {
				return -1
			}
			marker := b[offset+1]
			if marker == 0xff {
				offset++
				continue
			}
			if marker == 0xe1 && isExifHeader(b, offset+4) {
				return offset + 10
			}
			offset += 2 + (int64(b[offset+2])<<8 | int64(b[offset+3]))
		}
		return -1
	}
	if ascii(b, 0, "RIFF") && ascii(b, 8, "WEBP") {
		for offset := int64(12); offset+8 <= n; {
			size := byteAt(b, offset+4) | byteAt(b, offset+5)<<8 | byteAt(b, offset+6)<<16 | byteAt(b, offset+7)<<24
			// Some WebP files prefix the TIFF header with "Exif\0\0".
			if ascii(b, offset, "EXIF") {
				if isExifHeader(b, offset+8) {
					return offset + 14
				}
				return offset + 8
			}
			// RIFF chunks are padded to an even size.
			offset += 8 + size + size%2
		}
	}
	return -1
}

func isExifHeader(b []byte, offset int64) bool {
	return ascii(b, offset, "Exif") && offset+5 < int64(len(b)) && b[offset+4] == 0 && b[offset+5] == 0
}

func ascii(b []byte, offset int64, text string) bool {
	for i := range int64(len(text)) {
		if offset+i < 0 || offset+i >= int64(len(b)) || b[offset+i] != text[i] {
			return false
		}
	}
	return true
}
