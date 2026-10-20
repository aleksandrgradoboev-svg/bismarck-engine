// Package imagetest builds the image fixtures of pi's tools-images tests for
// the tests of durable/tools and durable/images. pi encodes them with Photon;
// here the standard PNG and JPEG encoders do.
package imagetest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
)

// RGB is one colour.
type RGB [3]uint8

// EncodeRGBA encodes w×h RGBA pixels as "png" or "jpeg" (quality 95).
func EncodeRGBA(rgba []byte, w, h int, format string) []byte {
	img := &image.NRGBA{Pix: rgba, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	var buf bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	}
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Pixels are the RGBA pixels of a w×h image: red, with a green top-left
// pixel.
func Pixels(w, h int) []byte {
	rgba := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		copy(rgba[i*4:], []byte{255, 0, 0, 255})
	}
	copy(rgba, []byte{0, 255, 0, 255})
	return rgba
}

// Encode is Pixels(w, h) encoded as format.
func Encode(w, h int, format string) []byte { return EncodeRGBA(Pixels(w, h), w, h, format) }

// Blocks is an image of block-pixel squares coloured row by row from colors.
func Blocks(colors [][]RGB, block int) (rgba []byte, w, h int) {
	w, h = len(colors[0])*block, len(colors)*block
	rgba = make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := colors[y/block][x/block]
			copy(rgba[(y*w+x)*4:], []byte{c[0], c[1], c[2], 255})
		}
	}
	return rgba, w, h
}

// NoisyPNG is noise, which neither PNG nor JPEG compresses well, so the byte
// limit decides.
func NoisyPNG(w, h int) []byte {
	rgba := make([]byte, w*h*4)
	seed := uint32(1)
	for i := range rgba {
		seed = seed*1103515245 + 12345
		if i%4 == 3 {
			rgba[i] = 255
		} else {
			rgba[i] = byte(seed >> 24)
		}
	}
	return EncodeRGBA(rgba, w, h, "png")
}

// BMP is a 24-bit blue BMP, which providers do not take inline.
func BMP(w, h int) []byte {
	row := (w*3 + 3) / 4 * 4
	b := make([]byte, 54+row*h)
	copy(b, "BM")
	le := binary.LittleEndian
	le.PutUint32(b[2:], uint32(len(b)))
	le.PutUint32(b[10:], 54)
	le.PutUint32(b[14:], 40)
	le.PutUint32(b[18:], uint32(w))
	le.PutUint32(b[22:], uint32(h))
	le.PutUint16(b[26:], 1)
	le.PutUint16(b[28:], 24)
	le.PutUint32(b[34:], uint32(row*h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			copy(b[54+y*row+x*3:], []byte{0, 0, 255})
		}
	}
	return b
}

// GIFData is a 1×1 GIF, base64.
const GIFData = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

// GIF is a 1×1 GIF with its logical screen size set to w×h.
func GIF(w, h int) []byte {
	b, err := base64.StdEncoding.DecodeString(GIFData)
	if err != nil {
		panic(err)
	}
	binary.LittleEndian.PutUint16(b[6:], uint16(w))
	binary.LittleEndian.PutUint16(b[8:], uint16(h))
	return b
}

// TIFF is a TIFF header whose first directory holds only Orientation.
func TIFF(orientation int, little bool) []byte {
	b := make([]byte, 26)
	var order binary.ByteOrder = binary.BigEndian
	copy(b, "MM")
	if little {
		order = binary.LittleEndian
		copy(b, "II")
	}
	order.PutUint16(b[2:], 42)
	order.PutUint32(b[4:], 8)
	order.PutUint16(b[8:], 1)
	order.PutUint16(b[10:], 0x0112)
	order.PutUint16(b[12:], 3)
	order.PutUint32(b[14:], 1)
	order.PutUint16(b[18:], uint16(orientation))
	return b
}

var exifHeader = []byte{0x45, 0x78, 0x69, 0x66, 0, 0}

// WithOrientation is jpeg with an APP1 segment whose EXIF says orientation,
// after an APP0 segment when app0.
func WithOrientation(jpegBytes []byte, orientation int, little, app0 bool) []byte {
	exif := append(append([]byte{}, exifHeader...), TIFF(orientation, little)...)
	app1 := append([]byte{0xff, 0xe1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}, exif...)
	out := append([]byte{}, jpegBytes[:2]...)
	if app0 {
		out = append(out, 0xff, 0xe0, 0, 16, 0x4a, 0x46, 0x49, 0x46, 0, 1, 1, 0, 0, 1, 0, 1, 0, 0)
	}
	out = append(out, app1...)
	return append(out, jpegBytes[2:]...)
}

// WebPWithOrientation is a RIFF WebP with a dummy "VP8 " chunk and an EXIF
// chunk saying orientation, its TIFF data bare or prefixed.
func WebPWithOrientation(orientation int, prefixed bool) []byte {
	chunk := func(kind string, data []byte) []byte {
		out := append([]byte(kind), byte(len(data)), byte(len(data)>>8), 0, 0)
		out = append(out, data...)
		if len(data)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	var exif []byte
	if prefixed {
		exif = append(exif, exifHeader...)
	}
	exif = append(exif, TIFF(orientation, true)...)
	body := append([]byte("WEBP"), chunk("VP8 ", []byte{1, 2, 3})...)
	body = append(body, chunk("EXIF", exif)...)
	return append(append([]byte("RIFF"), byte(len(body)), byte(len(body)>>8), 0, 0), body...)
}

// Decoded is an image's size and RGBA pixels.
type Decoded struct {
	Width, Height int
	RGBA          []byte
}

// Decode decodes base64 image data.
func Decode(data string) (Decoded, error) {
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return Decoded{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return Decoded{}, err
	}
	bounds := img.Bounds()
	out := Decoded{Width: bounds.Dx(), Height: bounds.Dy(), RGBA: make([]byte, bounds.Dx()*bounds.Dy()*4)}
	for y := 0; y < out.Height; y++ {
		for x := 0; x < out.Width; x++ {
			c := color.NRGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			copy(out.RGBA[(y*out.Width+x)*4:], []byte{c.R, c.G, c.B, c.A})
		}
	}
	return out, nil
}
