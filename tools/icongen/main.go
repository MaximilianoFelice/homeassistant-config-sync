// Command icongen draws the app's icon.png (128×128) and logo.png (250×100):
// a git branch-and-merge glyph on Home Assistant blue. Run it with
// `go run ./tools/icongen homeassistant_config_sync` after changing the design.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var (
	blue  = color.NRGBA{0x03, 0xa9, 0xf4, 0xff}
	white = color.NRGBA{0xff, 0xff, 0xff, 0xff}
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := write(filepath.Join(dir, "icon.png"), icon(128)); err != nil {
		fail(err)
	}
	if err := write(filepath.Join(dir, "logo.png"), logo()); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "icongen:", err)
	os.Exit(1)
}

func write(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return png.Encode(f, img)
}

// icon is a rounded blue square with the glyph in white.
func icon(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	fillRoundRect(img, 0, 0, s, s, s*0.22, blue)
	glyph(img, 0, 0, s, white)
	return img
}

// logo is the glyph in blue on a transparent, wider canvas.
func logo() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 250, 100))
	icn := icon(100)
	for y := range 100 {
		for x := range 100 {
			img.Set(x+75, y, icn.At(x, y))
		}
	}
	return img
}

// glyph draws two branches that split and merge back, like a sync between
// box and main, inside the square (x0, y0, size).
func glyph(img *image.NRGBA, x0, y0, size float64, c color.NRGBA) {
	p := func(fx, fy float64) (float64, float64) { return x0 + fx*size, y0 + fy*size }
	w := size * 0.07
	r := size * 0.085

	ax, ay := p(0.36, 0.22) // top of the trunk
	bx, by := p(0.36, 0.78) // bottom of the trunk
	cx, cy := p(0.66, 0.40) // side branch commit
	dx, dy := p(0.66, 0.60) // side branch commit

	thickLine(img, ax, ay, bx, by, w, c)
	thickLine(img, ax, ay+size*0.12, cx, cy, w, c)
	thickLine(img, cx, cy, dx, dy, w, c)
	thickLine(img, dx, dy, bx, by-size*0.12, w, c)
	for _, pt := range [][2]float64{{ax, ay}, {bx, by}, {cx, cy}, {dx, dy}} {
		fillCircle(img, pt[0], pt[1], r, c)
	}
}

func fillCircle(img *image.NRGBA, cx, cy, r float64, c color.NRGBA) {
	b := img.Bounds()
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			if !(image.Point{x, y}).In(b) {
				continue
			}
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			blend(img, x, y, c, clamp(r-d+0.5))
		}
	}
}

func thickLine(img *image.NRGBA, x1, y1, x2, y2, w float64, c color.NRGBA) {
	b := img.Bounds()
	minX, maxX := math.Min(x1, x2)-w, math.Max(x1, x2)+w
	minY, maxY := math.Min(y1, y2)-w, math.Max(y1, y2)+w
	dx, dy := x2-x1, y2-y1
	l2 := dx*dx + dy*dy
	for y := int(minY); y <= int(maxY); y++ {
		for x := int(minX); x <= int(maxX); x++ {
			if !(image.Point{x, y}).In(b) {
				continue
			}
			px, py := float64(x)+0.5, float64(y)+0.5
			t := math.Max(0, math.Min(1, ((px-x1)*dx+(py-y1)*dy)/l2))
			d := math.Hypot(px-(x1+t*dx), py-(y1+t*dy))
			blend(img, x, y, c, clamp(w/2-d+0.5))
		}
	}
}

func fillRoundRect(img *image.NRGBA, x0, y0, w, h, r float64, c color.NRGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			qx := math.Max(math.Max(x0+r-px, px-(x0+w-r)), 0)
			qy := math.Max(math.Max(y0+r-py, py-(y0+h-r)), 0)
			d := math.Hypot(qx, qy) - r
			blend(img, x, y, c, clamp(0.5-d))
		}
	}
}

func clamp(a float64) float64 { return math.Max(0, math.Min(1, a)) }

// blend paints c over the pixel with coverage a.
func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	if a <= 0 {
		return
	}
	dst := img.NRGBAAt(x, y)
	sa := a * float64(c.A) / 255
	da := float64(dst.A) / 255
	oa := sa + da*(1-sa)
	if oa == 0 {
		return
	}
	mix := func(s, d uint8) uint8 {
		return uint8((float64(s)*sa + float64(d)*da*(1-sa)) / oa)
	}
	img.SetNRGBA(x, y, color.NRGBA{mix(c.R, dst.R), mix(c.G, dst.G), mix(c.B, dst.B), uint8(oa * 255)})
}
