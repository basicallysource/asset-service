package model

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// renderPNG draws the mesh on a transparent square canvas: isometric view,
// orthographic projection, z-buffered, flat-shaded per triangle. Pure Go on
// purpose -- a picture of a part must not depend on which machine made it.
func renderPNG(m mesh, size int) ([]byte, error) {
	if len(m.tris) == 0 {
		return nil, fmt.Errorf("empty mesh")
	}

	// Isometric axes with Z up: u across the page, v up it, d toward the
	// viewer for depth ordering.
	const c30, s30 = 0.8660254037844387, 0.5
	project := func(p vec3) (u, v, d float64) {
		return (p.X - p.Y) * c30, (p.X+p.Y)*s30 - p.Z, p.X + p.Y + p.Z
	}

	// Fit the projected bounds to the canvas with a margin.
	minU, minV := math.Inf(1), math.Inf(1)
	maxU, maxV := math.Inf(-1), math.Inf(-1)
	for _, t := range m.tris {
		for _, p := range t {
			u, v, _ := project(p)
			minU, maxU = math.Min(minU, u), math.Max(maxU, u)
			minV, maxV = math.Min(minV, v), math.Max(maxV, v)
		}
	}
	span := math.Max(maxU-minU, maxV-minV)
	if span == 0 {
		return nil, fmt.Errorf("degenerate mesh")
	}
	margin := 0.08 * float64(size)
	scale := (float64(size) - 2*margin) / span
	offU := margin + (float64(size)-2*margin-(maxU-minU)*scale)/2 - minU*scale
	offV := margin + (float64(size)-2*margin-(maxV-minV)*scale)/2 - minV*scale

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	depth := make([]float64, size*size)
	for i := range depth {
		depth[i] = math.Inf(-1)
	}

	light := vec3{-0.35, -0.55, 0.75}.norm()
	const baseR, baseG, baseB = 152, 166, 178

	for _, t := range m.tris {
		// Two-sided shading: STL winding is unreliable, and a face lit as if
		// from behind would render black holes into a solid part.
		n := t[1].sub(t[0]).cross(t[2].sub(t[0])).norm()
		shade := 0.30 + 0.65*math.Abs(n.dot(light))

		var xs, ys, ds [3]float64
		for i, p := range t {
			u, v, d := project(p)
			xs[i], ys[i], ds[i] = u*scale+offU, v*scale+offV, d
		}

		x0 := int(math.Floor(math.Min(xs[0], math.Min(xs[1], xs[2]))))
		x1 := int(math.Ceil(math.Max(xs[0], math.Max(xs[1], xs[2]))))
		y0 := int(math.Floor(math.Min(ys[0], math.Min(ys[1], ys[2]))))
		y1 := int(math.Ceil(math.Max(ys[0], math.Max(ys[1], ys[2]))))
		x0, y0 = max(x0, 0), max(y0, 0)
		x1, y1 = min(x1, size-1), min(y1, size-1)

		area := (xs[1]-xs[0])*(ys[2]-ys[0]) - (ys[1]-ys[0])*(xs[2]-xs[0])
		if area == 0 {
			continue
		}

		col := color.NRGBA{
			R: uint8(baseR * shade), G: uint8(baseG * shade), B: uint8(baseB * shade), A: 255,
		}
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				px, py := float64(x)+0.5, float64(y)+0.5
				w0 := ((xs[1]-px)*(ys[2]-py) - (ys[1]-py)*(xs[2]-px)) / area
				w1 := ((xs[2]-px)*(ys[0]-py) - (ys[2]-py)*(xs[0]-px)) / area
				w2 := 1 - w0 - w1
				if w0 < 0 || w1 < 0 || w2 < 0 {
					continue
				}
				d := w0*ds[0] + w1*ds[1] + w2*ds[2]
				idx := y*size + x
				if d <= depth[idx] {
					continue
				}
				depth[idx] = d
				img.SetNRGBA(x, y, col)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
