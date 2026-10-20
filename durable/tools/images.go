package tools

import (
	"context"
	"encoding/base64"
)

// ImageLimits are the limits an image the model sees must keep; the defaults
// are the coding agent's.
type ImageLimits struct {
	// MaxWidth and MaxHeight are pixels.
	MaxWidth, MaxHeight int
	// MaxBytes is bytes of base64.
	MaxBytes int
}

// DefaultImageLimits: 2000x2000 pixels, 4.5 MiB of base64 (below
// Anthropic's 5 MB).
var DefaultImageLimits = ImageLimits{MaxWidth: 2000, MaxHeight: 2000, MaxBytes: 4.5 * 1024 * 1024}

// ImageSize is an image's width and height in pixels.
type ImageSize struct {
	Width, Height int
}

// PreparedImage is an image the model can take: a supported format, within
// the limits, as base64.
type PreparedImage struct {
	Data     string
	MimeType string
	// Resized is set when the image was resized: its size before and after,
	// for the model to map coordinates back.
	Resized *ImageResize
	// ConvertedFrom is the original format when it differs from MimeType:
	// BMP becomes PNG, a large PNG may become JPEG.
	ConvertedFrom string
}

// ImageResize is the size of a resized image before and after.
type ImageResize struct {
	From, To ImageSize
}

// ImageProcessor decodes, orients, resizes and re-encodes images so the
// model can take them. read uses one when given; without one, it passes
// supported images within the limits through as they are.
type ImageProcessor interface {
	// Prepare returns the image prepared to fit limits, or nil when it
	// cannot be decoded or made to fit.
	Prepare(ctx context.Context, bytes []byte, mimeType string, limits ImageLimits) (*PreparedImage, error)
}

// inlineImageTypes are the formats every provider takes inline; others, such
// as BMP, need converting.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func toBase64(bytes []byte) string { return base64.StdEncoding.EncodeToString(bytes) }

// base64Length is the length of the base64 of byteLength bytes.
func base64Length(byteLength int64) int64 { return (byteLength + 2) / 3 * 4 }
