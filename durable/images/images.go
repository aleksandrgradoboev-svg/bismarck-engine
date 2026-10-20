// Package images is an image processor for the durable read tool (pi
// durable/src/images): it turns images upright, scales them to the model's
// limits and re-encodes them until they fit, so read can return photos,
// screenshots and BMPs. It is pure Go: the standard decoders with
// golang.org/x/image's BMP and WebP, and the standard PNG and JPEG encoders,
// where pi uses Photon (Rust compiled to WebAssembly). The decisions — when
// an image passes through, its target size, the encodings tried and the
// shrinking — are pi's; the pixels are not byte-identical, as the resampling
// is bilinear where Photon's is Lanczos3.
package images

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	_ "image/gif" // registers the GIF decoder
	"image/jpeg"
	"image/png"
	"math"

	_ "golang.org/x/image/bmp"  // registers the BMP decoder
	_ "golang.org/x/image/webp" // registers the WebP decoder

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/tools"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/imaging"
)

// jpegQualities are tried in order.
var jpegQualities = []int{80, 70, 55, 40}

// inlineTypes are the formats providers take inline.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// NewImages is the image processor (pi createPhotonImages). An image already
// upright, in a format providers take inline and within the limits is
// returned as it is, once it decodes. Otherwise it is turned upright by its
// EXIF orientation, scaled to the maximum width and height, and encoded as
// PNG or JPEG at falling qualities (JPEG first for JPEG sources), then at
// smaller dimensions, 3/4 at a time, until the base64 fits. Formats providers
// do not take inline (BMP) become PNG. The work runs on the calling
// goroutine and is safe for concurrent use; a cancelled ctx stops it between
// encodings.
func NewImages() tools.ImageProcessor { return processor{} }

type processor struct{}

func base64Length(n int) int { return (n + 2) / 3 * 4 }

// jsRound is JS Math.round for non-negative x.
func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

func (processor) Prepare(ctx context.Context, data []byte, mimeType string, limits tools.ImageLimits) (*tools.PreparedImage, error) {
	orientation := ExifOrientation(data)
	// Decoded even when it is returned as it is: a damaged file is an error,
	// not an image the provider rejects.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil
	}
	bounds := img.Bounds()
	if inlineTypes[mimeType] && orientation == 1 &&
		bounds.Dx() <= limits.MaxWidth && bounds.Dy() <= limits.MaxHeight &&
		base64Length(len(data)) <= limits.MaxBytes {
		return &tools.PreparedImage{Data: base64.StdEncoding.EncodeToString(data), MimeType: mimeType}, nil
	}
	if orientation != 1 {
		img = imaging.ApplyOrientation(img, orientation)
	}
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	scale := math.Min(1, math.Min(float64(limits.MaxWidth)/float64(width), float64(limits.MaxHeight)/float64(height)))
	target := tools.ImageSize{Width: max(1, jsRound(float64(width)*scale)), Height: max(1, jsRound(float64(height)*scale))}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if prepared := encodeWithin(img, target, limits.MaxBytes, mimeType == "image/jpeg"); prepared != nil {
			if prepared.MimeType != mimeType {
				prepared.ConvertedFrom = mimeType
			}
			if target.Width != width || target.Height != height {
				prepared.Resized = &tools.ImageResize{From: tools.ImageSize{Width: width, Height: height}, To: target}
			}
			return prepared, nil
		}
		if target.Width == 1 && target.Height == 1 {
			return nil, nil
		}
		target = tools.ImageSize{
			Width:  max(1, int(math.Floor(float64(target.Width)*0.75))),
			Height: max(1, int(math.Floor(float64(target.Height)*0.75))),
		}
	}
}

// encodeWithin is img at size in the first encoding whose base64 fits
// maxBytes: PNG, then JPEG at falling qualities, or the JPEG qualities first
// when jpegFirst; nil if none fits.
func encodeWithin(img image.Image, size tools.ImageSize, maxBytes int, jpegFirst bool) *tools.PreparedImage {
	scaled := img
	if b := img.Bounds(); size.Width != b.Dx() || size.Height != b.Dy() {
		scaled = imaging.BilinearResize(img, size.Width, size.Height)
	}
	type candidate struct {
		mimeType string
		encode   func(*bytes.Buffer) error
	}
	pngCandidate := candidate{"image/png", func(buf *bytes.Buffer) error { return png.Encode(buf, scaled) }}
	var candidates []candidate
	if !jpegFirst {
		candidates = append(candidates, pngCandidate)
	}
	for _, quality := range jpegQualities {
		candidates = append(candidates, candidate{"image/jpeg", func(buf *bytes.Buffer) error {
			return jpeg.Encode(buf, scaled, &jpeg.Options{Quality: quality})
		}})
	}
	if jpegFirst {
		candidates = append(candidates, pngCandidate)
	}
	var buf bytes.Buffer
	for _, c := range candidates {
		buf.Reset()
		if c.encode(&buf) != nil {
			continue
		}
		if base64Length(buf.Len()) <= maxBytes {
			return &tools.PreparedImage{Data: base64.StdEncoding.EncodeToString(buf.Bytes()), MimeType: c.mimeType}
		}
	}
	return nil
}
