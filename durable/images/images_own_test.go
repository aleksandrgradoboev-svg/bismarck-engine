package images

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/imagetest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/tools"
)

// Cases beyond pi's tests: each pins a decision of pi's prepare that no pi
// case reaches.

func TestImagesPassAnImageWhoseBase64IsExactlyTheLimitThrough(t *testing.T) {
	gif := imagetest.GIF(1, 1)
	limits := tools.DefaultImageLimits
	limits.MaxBytes = base64Length(len(gif))
	result := prepare(t, gif, "image/gif", limits)
	if result == nil || result.MimeType != "image/gif" || result.Data != imagetest.GIFData {
		t.Fatalf("result = %+v, want the GIF as it is", result)
	}
}

func TestImagesRoundTheScaledSideToTheNearestPixel(t *testing.T) {
	// 1000 · 2000/3000 = 666.67, which rounds to 667.
	result := prepare(t, imagetest.Encode(3000, 1000, "png"), "image/png", tools.DefaultImageLimits)
	if result == nil || result.Resized == nil || result.Resized.To != (tools.ImageSize{Width: 2000, Height: 667}) {
		t.Fatalf("result = %+v", result)
	}
	// And the width of a portrait.
	result = prepare(t, imagetest.Encode(1000, 3000, "png"), "image/png", tools.DefaultImageLimits)
	if result == nil || result.Resized == nil || result.Resized.To != (tools.ImageSize{Width: 667, Height: 2000}) {
		t.Fatalf("portrait result = %+v", result)
	}
}

func TestImagesShrinkByThreeQuartersAtATime(t *testing.T) {
	limits := tools.DefaultImageLimits
	limits.MaxBytes = 200_000
	result := prepare(t, imagetest.NoisyPNG(2000, 2000), "image/png", limits)
	if result == nil || result.Resized == nil {
		t.Fatalf("result = %+v", result)
	}
	width := 2000
	for width > result.Resized.To.Width {
		width = max(1, int(math.Floor(float64(width)*0.75)))
	}
	if width != result.Resized.To.Width {
		t.Fatalf("width %d is not a step of 3/4 from 2000", result.Resized.To.Width)
	}
}

func TestImagesKeepShrinkingTheLongSideOnceTheShortOneIsOnePixel(t *testing.T) {
	limits := tools.DefaultImageLimits
	limits.MaxBytes = 400
	result := prepare(t, imagetest.NoisyPNG(6000, 2), "image/png", limits)
	if result == nil || result.Resized == nil || result.Resized.To.Height != 1 || result.Resized.To.Width >= 2000 {
		t.Fatalf("result = %+v", result)
	}
}

func TestImagesStopWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewImages().Prepare(ctx, imagetest.Encode(4000, 1000, "png"), "image/png", tools.DefaultImageLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestImagesTryTheLowerJPEGQualitiesBeforeShrinking(t *testing.T) {
	png := imagetest.NoisyPNG(300, 300)
	img, _, err := image.Decode(bytes.NewReader(png))
	must(t, err)
	var low bytes.Buffer
	must(t, jpeg.Encode(&low, img, &jpeg.Options{Quality: 40}))
	limits := tools.DefaultImageLimits
	limits.MaxBytes = base64Length(low.Len())
	result := prepare(t, png, "image/png", limits)
	if result == nil || result.MimeType != "image/jpeg" || result.Resized != nil {
		t.Fatalf("result = %+v, want a JPEG at its size", result)
	}
}
