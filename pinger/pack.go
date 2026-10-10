package pinger

import (
	"image"
)

// PackImage packs every visible pixel of img into canvas target values in the
// same 64-bit format buildTargets uses, but offset by (ox, oy) so the packed
// X/Y are absolute canvas coordinates. Pixels whose alpha is below minAlpha
// are skipped. The image origin is honoured, so SubImages work too.
func PackImage(img *image.NRGBA, ox, oy, minAlpha int) []uint64 {
	b := img.Bounds()
	out := make([]uint64, 0, b.Dx()*b.Dy()/4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[(y-b.Min.Y)*img.Stride : (y-b.Min.Y)*img.Stride+b.Dx()*4]
		for x := b.Min.X; x < b.Max.X; x++ {
			i := (x - b.Min.X) * 4
			r, g, bl, a := row[i], row[i+1], row[i+2], row[i+3]
			if int(a) < minAlpha {
				continue
			}
			out = append(out, uint64(ox+x)<<48|uint64(oy+y)<<32|uint64(bl)<<24|uint64(g)<<16|uint64(r)<<8|uint64(a))
		}
	}
	return out
}
