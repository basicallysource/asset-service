package model

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/png"
	"math"
	"testing"
)

// tetrahedron returns a small closed mesh, offset so tests see the
// translation working.
func tetrahedron(offset vec3) mesh {
	a := vec3{0, 0, 0}.add(offset)
	b := vec3{10, 0, 0}.add(offset)
	c := vec3{0, 10, 0}.add(offset)
	d := vec3{0, 0, 10}.add(offset)
	return mesh{tris: [][3]vec3{{a, b, c}, {a, b, d}, {a, c, d}, {b, c, d}}}
}

func binarySTL(m mesh) []byte { return writeBinarySTL(m) }

func asciiSTL(m mesh) []byte {
	var buf bytes.Buffer
	buf.WriteString("solid test\n")
	for _, t := range m.tris {
		buf.WriteString(" facet normal 0 0 0\n  outer loop\n")
		for _, v := range t {
			fmt.Fprintf(&buf, "   vertex %g %g %g\n", v.X, v.Y, v.Z)
		}
		buf.WriteString("  endloop\n endfacet\n")
	}
	buf.WriteString("endsolid test\n")
	return buf.Bytes()
}

func TestParseBinarySTL(t *testing.T) {
	m, err := parseSTL(binarySTL(tetrahedron(vec3{5, -3, 2})))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.tris) != 4 {
		t.Fatalf("triangles = %d, want 4", len(m.tris))
	}
	lo, hi := m.bounds()
	if lo.Z != 2 || hi.Z != 12 {
		t.Errorf("z bounds = %v..%v, want 2..12", lo.Z, hi.Z)
	}
}

func TestParseASCIISTL(t *testing.T) {
	m, err := parseSTL(asciiSTL(tetrahedron(vec3{})))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.tris) != 4 {
		t.Fatalf("triangles = %d, want 4", len(m.tris))
	}
}

// A binary STL whose exporter wrote "solid" into the header must still be
// read as binary; the arithmetic decides, not the prefix.
func TestBinarySTLWithSolidHeader(t *testing.T) {
	data := binarySTL(tetrahedron(vec3{}))
	copy(data, "solid binary-anyway")
	m, err := parseSTL(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.tris) != 4 {
		t.Fatalf("triangles = %d, want 4", len(m.tris))
	}
}

func TestParseSTLRefusesJunk(t *testing.T) {
	if _, err := parseSTL([]byte("not a model at all")); err == nil {
		t.Fatal("junk parsed as a mesh")
	}
	// A plausible triangle count with a body that does not match it.
	junk := make([]byte, 84+10)
	binary.LittleEndian.PutUint32(junk[80:], 100)
	if _, err := parseSTL(junk); err == nil {
		t.Fatal("truncated binary parsed as a mesh")
	}
}

func TestRenderDrawsSomething(t *testing.T) {
	data, err := renderPNG(tetrahedron(vec3{100, 100, 0}), 256)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("canvas = %v, want 256x256", img.Bounds())
	}
	painted := 0
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0 {
				painted++
			}
		}
	}
	// The part fills most of the canvas; a sliver means projection or fit is
	// broken, zero means nothing rasterized at all.
	if painted < 256*256/10 {
		t.Fatalf("painted %d of %d pixels; render is (nearly) empty", painted, 256*256)
	}
}

func TestPrepareMeshDropsAndCenters(t *testing.T) {
	dir := t.TempDir()
	path, err := prepareMesh(tetrahedron(vec3{500, -200, 40}), dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseSTL(raw)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := m.bounds()
	if math.Abs(lo.Z) > 1e-4 {
		t.Errorf("min z = %g, want 0 (dropped onto the plate)", lo.Z)
	}
	if cx := (lo.X + hi.X) / 2; math.Abs(cx-bedCenter) > 1e-3 {
		t.Errorf("center x = %g, want %g", cx, bedCenter)
	}
	if cy := (lo.Y + hi.Y) / 2; math.Abs(cy-bedCenter) > 1e-3 {
		t.Errorf("center y = %g, want %g", cy, bedCenter)
	}
}

func TestSupportedIsAboutTheTypeAlone(t *testing.T) {
	if !Supported("model/stl") || !Supported("application/sla") {
		t.Error("stl types must be supported regardless of any installed tool")
	}
	if Supported("image/png") || Supported("application/octet-stream") {
		t.Error("non-model types must not be")
	}
}
