package model

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// vec3 is a point or direction in model space, millimetres, Z up.
type vec3 struct{ X, Y, Z float64 }

func (a vec3) sub(b vec3) vec3 { return vec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a vec3) add(b vec3) vec3 { return vec3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a vec3) cross(b vec3) vec3 {
	return vec3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}
func (a vec3) dot(b vec3) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a vec3) norm() vec3 {
	l := math.Sqrt(a.dot(a))
	if l == 0 {
		return a
	}
	return vec3{a.X / l, a.Y / l, a.Z / l}
}

// mesh is triangle soup, which is all an STL is. Normals are not kept: files
// lie about them often enough that anything needing one recomputes it.
type mesh struct {
	tris [][3]vec3
}

// maxTriangles bounds what one file may ask this process to hold. Fifty
// million triangles is a scan of a building, not a part.
const maxTriangles = 50_000_000

// parseSTL reads either encoding of STL. The binary layout is fixed, so the
// reliable test is arithmetic: a binary file's length is exactly its header
// plus fifty bytes a triangle. "solid" at the front proves nothing -- binary
// exporters write it too.
func parseSTL(data []byte) (mesh, error) {
	if len(data) >= 84 {
		count := binary.LittleEndian.Uint32(data[80:84])
		if count > 0 && count <= maxTriangles && int64(len(data)) == 84+int64(count)*50 {
			return parseBinarySTL(data, count)
		}
	}
	if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("solid")) {
		return parseASCIISTL(data)
	}
	return mesh{}, fmt.Errorf("not an STL file")
}

func parseBinarySTL(data []byte, count uint32) (mesh, error) {
	m := mesh{tris: make([][3]vec3, 0, count)}
	off := 84
	for range count {
		off += 12 // the stored normal; recomputed when needed
		var tri [3]vec3
		for v := range 3 {
			tri[v] = vec3{
				X: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))),
				Y: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+4:]))),
				Z: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+8:]))),
			}
			off += 12
		}
		m.tris = append(m.tris, tri)
		off += 2 // attribute byte count
	}
	return m, nil
}

func parseASCIISTL(data []byte) (mesh, error) {
	var m mesh
	var tri [3]vec3
	verts := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 4 && fields[0] == "vertex" {
			x, err1 := strconv.ParseFloat(fields[1], 64)
			y, err2 := strconv.ParseFloat(fields[2], 64)
			z, err3 := strconv.ParseFloat(fields[3], 64)
			if err1 != nil || err2 != nil || err3 != nil {
				return mesh{}, fmt.Errorf("bad vertex line %q", sc.Text())
			}
			if verts < 3 {
				tri[verts] = vec3{x, y, z}
			}
			verts++
			continue
		}
		if len(fields) >= 1 && fields[0] == "endfacet" {
			if verts != 3 {
				return mesh{}, fmt.Errorf("facet with %d vertices", verts)
			}
			if len(m.tris) >= maxTriangles {
				return mesh{}, fmt.Errorf("more than %d triangles", maxTriangles)
			}
			m.tris = append(m.tris, tri)
			verts = 0
		}
	}
	if err := sc.Err(); err != nil {
		return mesh{}, err
	}
	if len(m.tris) == 0 {
		return mesh{}, fmt.Errorf("no facets")
	}
	return m, nil
}

// bounds is the axis-aligned box around every vertex.
func (m mesh) bounds() (min, max vec3) {
	min = vec3{math.Inf(1), math.Inf(1), math.Inf(1)}
	max = vec3{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
	for _, t := range m.tris {
		for _, v := range t {
			min.X, min.Y, min.Z = math.Min(min.X, v.X), math.Min(min.Y, v.Y), math.Min(min.Z, v.Z)
			max.X, max.Y, max.Z = math.Max(max.X, v.X), math.Max(max.Y, v.Y), math.Max(max.Z, v.Z)
		}
	}
	return min, max
}

// translated returns the mesh moved by d.
func (m mesh) translated(d vec3) mesh {
	out := mesh{tris: make([][3]vec3, len(m.tris))}
	for i, t := range m.tris {
		out.tris[i] = [3]vec3{t[0].add(d), t[1].add(d), t[2].add(d)}
	}
	return out
}

// writeBinarySTL encodes the mesh in the binary layout, normals recomputed.
func writeBinarySTL(m mesh) []byte {
	buf := make([]byte, 84, 84+len(m.tris)*50)
	copy(buf, "asset-service prepared mesh")
	binary.LittleEndian.PutUint32(buf[80:], uint32(len(m.tris)))
	var scratch [50]byte
	for _, t := range m.tris {
		n := t[1].sub(t[0]).cross(t[2].sub(t[0])).norm()
		coords := [12]float64{n.X, n.Y, n.Z,
			t[0].X, t[0].Y, t[0].Z, t[1].X, t[1].Y, t[1].Z, t[2].X, t[2].Y, t[2].Z}
		for i, c := range coords {
			binary.LittleEndian.PutUint32(scratch[i*4:], math.Float32bits(float32(c)))
		}
		scratch[48], scratch[49] = 0, 0
		buf = append(buf, scratch[:]...)
	}
	return buf
}
