// Package imaging is the pixel work of the image tools: EXIF orientation and
// downscaling, shared by the coding read tool's image pipeline and the
// durable image processor. Both turn an image upright the way pi does; the
// resampling is a pure-Go bilinear one where pi uses Photon's Lanczos3.
package imaging

import (
	"image"
	"image/color"
)

// BilinearResize downscales src to tw×th using bilinear interpolation.
func BilinearResize(src image.Image, tw, th int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	if sw == 0 || sh == 0 {
		return dst
	}
	xRatio := float64(sw-1) / float64(max1(tw-1))
	yRatio := float64(sh-1) / float64(max1(th-1))
	for y := 0; y < th; y++ {
		fy := float64(y) * yRatio
		y0 := int(fy)
		dy := fy - float64(y0)
		for x := 0; x < tw; x++ {
			fx := float64(x) * xRatio
			x0 := int(fx)
			dx := fx - float64(x0)
			r00, g00, b00, a00 := at(src, sb.Min.X+x0, sb.Min.Y+y0)
			r10, g10, b10, a10 := at(src, sb.Min.X+x0+1, sb.Min.Y+y0)
			r01, g01, b01, a01 := at(src, sb.Min.X+x0, sb.Min.Y+y0+1)
			r11, g11, b11, a11 := at(src, sb.Min.X+x0+1, sb.Min.Y+y0+1)
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(lerp2(r00, r10, r01, r11, dx, dy)),
				G: uint8(lerp2(g00, g10, g01, g11, dx, dy)),
				B: uint8(lerp2(b00, b10, b01, b11, dx, dy)),
				A: uint8(lerp2(a00, a10, a01, a11, dx, dy)),
			})
		}
	}
	return dst
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func at(img image.Image, x, y int) (r, g, b, a uint32) {
	bb := img.Bounds()
	if x >= bb.Max.X {
		x = bb.Max.X - 1
	}
	if y >= bb.Max.Y {
		y = bb.Max.Y - 1
	}
	return img.At(x, y).RGBA()
}

func lerp2(c00, c10, c01, c11 uint32, dx, dy float64) uint32 {
	top := float64(c00>>8)*(1-dx) + float64(c10>>8)*dx
	bot := float64(c01>>8)*(1-dx) + float64(c11>>8)*dx
	return uint32(top*(1-dy) + bot*dy)
}

// ApplyOrientation rotates/flips img per the EXIF orientation value (1-8),
// matching pi's applyExifOrientation pixel mapping.
func ApplyOrientation(img image.Image, orientation int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	transform := func(dstW, dstH int, mapXY func(x, y int) (int, int)) image.Image {
		dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
		for y := 0; y < dstH; y++ {
			for x := 0; x < dstW; x++ {
				sx, sy := mapXY(x, y)
				dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
			}
		}
		return dst
	}
	switch orientation {
	case 2: // flip horizontal
		return transform(w, h, func(x, y int) (int, int) { return w - 1 - x, y })
	case 3: // rotate 180
		return transform(w, h, func(x, y int) (int, int) { return w - 1 - x, h - 1 - y })
	case 4: // flip vertical
		return transform(w, h, func(x, y int) (int, int) { return x, h - 1 - y })
	case 5: // transpose
		return transform(h, w, func(x, y int) (int, int) { return y, x })
	case 6: // rotate 90 CW
		return transform(h, w, func(x, y int) (int, int) { return y, h - 1 - x })
	case 7: // transverse
		return transform(h, w, func(x, y int) (int, int) { return w - 1 - y, h - 1 - x })
	case 8: // rotate 90 CCW
		return transform(h, w, func(x, y int) (int, int) { return w - 1 - y, x })
	default:
		return img
	}
}
