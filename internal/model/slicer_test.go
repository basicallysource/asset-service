package model

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func TestResolveProfileFlattensInherits(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "process"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, v map[string]any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, "process", name+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base", map[string]any{"layer_height": "0.2", "walls": "3"})
	write("leaf", map[string]any{"inherits": "base", "walls": "4"})

	p, err := resolveProfile(dir, "process", "leaf")
	if err != nil {
		t.Fatal(err)
	}
	if p["layer_height"] != "0.2" {
		t.Errorf("layer_height = %v, want inherited 0.2", p["layer_height"])
	}
	if p["walls"] != "4" {
		t.Errorf("walls = %v, want the leaf's 4", p["walls"])
	}
}

// A 3MF as Orca writes it, reduced to the parts the parser reads.
func fake3MF(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	si, _ := w.Create("Metadata/slice_info.config")
	si.Write([]byte(`<config>
  <plate>
    <metadata key="support_used" value="true"/>
    <filament id="1" used_g="10.50" used_m="3.51"/>
  </plate>
</config>`))

	gc, _ := w.Create("Metadata/plate_1.gcode")
	gc.Write([]byte(`; filament used [cm3] = 12.50
; total estimated time: 1h 2m 3s
M83
; FEATURE:Outer wall
G1 X1 Y1 E5.0
; FEATURE:Support
G1 X2 Y2 E2.5
; FEATURE:Inner wall
G1 X3 Y3 E2.5
`))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "out.3mf")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParse3MF(t *testing.T) {
	m, err := parse3MF(fake3MF(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Grams != 10.5 {
		t.Errorf("grams = %v, want 10.5", m.Grams)
	}
	if !m.SupportUsed {
		t.Error("support_used not read")
	}
	if m.CM3 != 12.5 {
		t.Errorf("cm3 = %v, want 12.5", m.CM3)
	}
	if m.PrintSeconds != 3723 {
		t.Errorf("print_seconds = %d, want 3723", m.PrintSeconds)
	}
	// A quarter of the extrusion was the support feature.
	if m.SupportGrams != 2.63 {
		t.Errorf("support_grams = %v, want 2.63 (a quarter of 10.5)", m.SupportGrams)
	}
}

func TestParseEstimatedTime(t *testing.T) {
	if got := parseEstimatedTime("; total estimated time: 2m 5s"); got != 125 {
		t.Errorf("got %d, want 125", got)
	}
	if got := parseEstimatedTime("; total estimated time: 3h"); got != 10800 {
		t.Errorf("got %d, want 10800", got)
	}
}

// The full ladder, only where a real slicer is installed.
func TestLadderWithRealSlicer(t *testing.T) {
	opts := OptionsFromEnv()
	if !Available(opts) {
		t.Skip("OrcaSlicer is not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "part.stl")
	if err := os.WriteFile(path, writeBinarySTL(tetrahedron(vec3{})), 0o644); err != nil {
		t.Fatal(err)
	}
	ladder, err := Ladder(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 3 {
		t.Fatalf("ladder has %d renditions, want render + slice + slice-support", len(ladder))
	}
	var report Metrics
	if err := json.Unmarshal(ladder[1].Bytes, &report); err != nil {
		t.Fatal(err)
	}
	if report.Grams <= 0 {
		t.Errorf("grams = %v, want > 0", report.Grams)
	}
}
