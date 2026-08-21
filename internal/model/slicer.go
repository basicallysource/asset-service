package model

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The slicing settings, fixed on purpose. A report is only comparable to
// another if both were sliced the same way, so these are constants rather
// than parameters, and every report carries them so a number can never be
// read apart from what produced it. The printer choice barely affects grams.
const (
	printerProfile  = "Bambu Lab A1 0.4 nozzle"
	processProfile  = "0.20mm Standard @BBL A1"
	filamentProfile = "Bambu PLA Matte @BBL A1"

	infillDensity = "15%"
	infillPattern = "adaptivecubic"

	supportType         = "normal(auto)"
	supportThresholdDeg = "10"
)

// sliceTimeout bounds one slicer run. A part slices in seconds; a slicer that
// has taken a quarter of an hour is stuck, not busy.
const sliceTimeout = 15 * time.Minute

// bedCenter is the middle of the oversized virtual bed built below.
const bedCenter = 300.0

// Metrics is what one slice reports: the slicer's own numbers, not estimates.
type Metrics struct {
	Support      bool    `json:"support"`
	Grams        float64 `json:"grams"`
	SupportGrams float64 `json:"support_grams"`
	CM3          float64 `json:"cm3"`
	PrintSeconds int     `json:"print_seconds"`
	SupportUsed  bool    `json:"support_used"`

	Settings SliceSettings `json:"settings"`
}

// SliceSettings names what produced the numbers.
type SliceSettings struct {
	Printer             string `json:"printer"`
	Process             string `json:"process"`
	Filament            string `json:"filament"`
	InfillDensity       string `json:"infill_density"`
	InfillPattern       string `json:"infill_pattern"`
	SupportType         string `json:"support_type,omitempty"`
	SupportThresholdDeg string `json:"support_threshold_deg,omitempty"`
}

func settingsFor(support bool) SliceSettings {
	s := SliceSettings{
		Printer:       printerProfile,
		Process:       processProfile,
		Filament:      filamentProfile,
		InfillDensity: infillDensity,
		InfillPattern: infillPattern,
	}
	if support {
		s.SupportType = supportType
		s.SupportThresholdDeg = supportThresholdDeg
	}
	return s
}

// profileSet is the four prepared profile files one slice run loads.
type profileSet struct {
	machine, processOff, processOn, filament string
}

// resolveProfile reads dir/<kind>/<name>.json and flattens its `inherits`
// chain, nearest definition winning.
func resolveProfile(dir, kind, name string) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(dir, kind, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("profile %s/%s: %w", kind, name, err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("profile %s/%s: %w", kind, name, err)
	}
	inherits, _ := p["inherits"].(string)
	if inherits == "" {
		return p, nil
	}
	base, err := resolveProfile(dir, kind, inherits)
	if err != nil {
		return nil, err
	}
	for k, v := range p {
		base[k] = v
	}
	return base, nil
}

func writeProfile(path string, p map[string]any) error {
	data, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// buildProfiles prepares the profiles a slice loads, in workDir.
//
// The machine keeps its leaf profile -- Orca resolves its `inherits` -- with
// only the bed overridden to a large virtual one: filament grams are
// bed-independent, and the CLI rejects parts near the real bed's edge with a
// margin the GUI never demands. Process and filament are flattened, and the
// process gets a variant per support answer.
func buildProfiles(profilesDir, workDir string) (profileSet, error) {
	var set profileSet

	machineRaw, err := os.ReadFile(filepath.Join(profilesDir, "machine", printerProfile+".json"))
	if err != nil {
		return set, fmt.Errorf("machine profile: %w (is OrcaSlicer installed?)", err)
	}
	var machine map[string]any
	if err := json.Unmarshal(machineRaw, &machine); err != nil {
		return set, fmt.Errorf("machine profile: %w", err)
	}
	machine["printable_area"] = []string{"0x0", "600x0", "600x600", "0x600"}
	machine["printable_height"] = "600"
	machine["bed_exclude_area"] = []string{}
	set.machine = filepath.Join(workDir, "machine.json")
	if err := writeProfile(set.machine, machine); err != nil {
		return set, err
	}

	proc, err := resolveProfile(profilesDir, "process", processProfile)
	if err != nil {
		return set, err
	}
	delete(proc, "inherits")
	proc["name"] = "asset-service process"
	proc["sparse_infill_density"] = infillDensity
	proc["sparse_infill_pattern"] = infillPattern
	proc["skirt_loops"] = "0"

	proc["enable_support"] = "0"
	set.processOff = filepath.Join(workDir, "process.json")
	if err := writeProfile(set.processOff, proc); err != nil {
		return set, err
	}

	proc["enable_support"] = "1"
	proc["support_type"] = supportType
	proc["support_threshold_angle"] = supportThresholdDeg
	set.processOn = filepath.Join(workDir, "process_support.json")
	if err := writeProfile(set.processOn, proc); err != nil {
		return set, err
	}

	fil, err := resolveProfile(profilesDir, "filament", filamentProfile)
	if err != nil {
		return set, err
	}
	delete(fil, "inherits")
	fil["name"] = "asset-service filament"
	set.filament = filepath.Join(workDir, "filament.json")
	if err := writeProfile(set.filament, fil); err != nil {
		return set, err
	}
	return set, nil
}

// prepareMesh does what the slicer's GUI does on import and its CLI does not:
// drop the part onto the plate and center it on the bed. Parts arrive
// carrying CAD assembly coordinates -- floating, sunk, far off origin -- and
// the CLI rejects those outright. The modeled orientation is kept: it is the
// print orientation.
func prepareMesh(m mesh, workDir string) (string, error) {
	lo, hi := m.bounds()
	moved := m.translated(vec3{
		X: bedCenter - (lo.X+hi.X)/2,
		Y: bedCenter - (lo.Y+hi.Y)/2,
		Z: -lo.Z,
	})
	path := filepath.Join(workDir, "prepared.stl")
	if err := os.WriteFile(path, writeBinarySTL(moved), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// slice runs OrcaSlicer once and reads its own numbers back out of the 3MF it
// exported.
func slice(ctx context.Context, opts Options, profiles profileSet, stlPath string, support bool) (Metrics, error) {
	process := profiles.processOff
	if support {
		process = profiles.processOn
	}
	outDir, err := os.MkdirTemp(filepath.Dir(stlPath), "slice-*")
	if err != nil {
		return Metrics{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, sliceTimeout)
	defer cancel()

	args := []string{
		"--load-settings", profiles.machine + ";" + process,
		"--load-filaments", profiles.filament,
		"--orient", "0", "--arrange", "1", "--slice", "0",
		"--export-3mf", "out.3mf", "--outputdir", outDir, stlPath,
	}
	bin := opts.Bin
	// The Linux build needs an X display even headless. Where there is none,
	// xvfb-run provides one; its absence is a real error worth reading.
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {
		if _, err := exec.LookPath("xvfb-run"); err != nil {
			return Metrics{}, fmt.Errorf("no DISPLAY and no xvfb-run; the slicer cannot start")
		}
		args = append([]string{"-a", bin}, args...)
		bin = "xvfb-run"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// The slicer writes log files into its working directory; keep those in
	// the scratch dir rather than wherever this process happens to be.
	cmd.Dir = outDir
	output, runErr := cmd.CombinedOutput()

	threeMF := filepath.Join(outDir, "out.3mf")
	if _, statErr := os.Stat(threeMF); runErr != nil || statErr != nil {
		tail := output
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return Metrics{}, fmt.Errorf("slicer failed (%v): %s", runErr, strings.TrimSpace(string(tail)))
	}

	metrics, err := parse3MF(threeMF)
	if err != nil {
		return Metrics{}, err
	}
	metrics.Support = support
	metrics.Settings = settingsFor(support)
	return metrics, nil
}

var extrudeRE = regexp.MustCompile(` E(-?[0-9.]+)`)

// parse3MF reads the slicer's own answer out of an exported 3MF: total grams
// from the slice report, the support share from per-feature G-code, volume
// and time from the G-code header.
func parse3MF(path string) (Metrics, error) {
	var m Metrics
	z, err := zip.OpenReader(path)
	if err != nil {
		return m, fmt.Errorf("read 3mf: %w", err)
	}
	defer z.Close()

	var sliceInfo, gcodeName string
	for _, f := range z.File {
		if f.Name == "Metadata/slice_info.config" {
			sliceInfo = f.Name
		}
		if strings.HasSuffix(f.Name, ".gcode") && gcodeName == "" {
			gcodeName = f.Name
		}
	}
	if sliceInfo == "" {
		return m, fmt.Errorf("3mf has no slice_info.config")
	}

	info, err := readZipFile(z, sliceInfo)
	if err != nil {
		return m, err
	}
	for _, line := range strings.Split(string(info), "\n") {
		if strings.Contains(line, "support_used") && strings.Contains(line, `value="true"`) {
			m.SupportUsed = true
		}
		if i := strings.Index(line, `used_g="`); i >= 0 {
			rest := line[i+len(`used_g="`):]
			if j := strings.Index(rest, `"`); j > 0 {
				if g, err := strconv.ParseFloat(rest[:j], 64); err == nil {
					m.Grams += g
				}
			}
		}
	}

	if gcodeName != "" {
		f, err := z.Open(gcodeName)
		if err != nil {
			return m, err
		}
		defer f.Close()

		var eTotal, eSupport float64
		curSupport := false
		relative := true
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			t := sc.Text()
			switch {
			case strings.HasPrefix(t, "; FEATURE:"):
				curSupport = strings.Contains(strings.ToLower(t), "support")
			case strings.HasPrefix(t, "M82"):
				relative = false
			case strings.HasPrefix(t, "M83"):
				relative = true
			case strings.Contains(t, "filament used [cm3]"):
				if _, after, ok := strings.Cut(t, "="); ok {
					if v, err := strconv.ParseFloat(strings.TrimSpace(after), 64); err == nil {
						m.CM3 = v
					}
				}
			case strings.Contains(t, "total estimated time:"):
				m.PrintSeconds = parseEstimatedTime(t)
			case strings.HasPrefix(t, "G1"), strings.HasPrefix(t, "G0"):
				if !relative {
					continue
				}
				if match := extrudeRE.FindStringSubmatch(t); match != nil {
					if e, err := strconv.ParseFloat(match[1], 64); err == nil && e > 0 {
						eTotal += e
						if curSupport {
							eSupport += e
						}
					}
				}
			}
		}
		if err := sc.Err(); err != nil {
			return m, err
		}
		if eTotal > 0 {
			m.SupportGrams = round2(m.Grams * eSupport / eTotal)
		}
	}

	m.Grams = round2(m.Grams)
	m.CM3 = round2(m.CM3)
	return m, nil
}

func readZipFile(z *zip.ReadCloser, name string) ([]byte, error) {
	f, err := z.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf strings.Builder
	if _, err := bufio.NewReader(f).WriteTo(&buf); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// parseEstimatedTime turns "; total estimated time: 1h 2m 3s" into seconds.
func parseEstimatedTime(line string) int {
	_, after, _ := strings.Cut(line, "total estimated time:")
	seconds := 0
	for _, tok := range strings.Fields(strings.ReplaceAll(after, ";", " ")) {
		unit := tok[len(tok)-1]
		n, err := strconv.Atoi(tok[:len(tok)-1])
		if err != nil {
			continue
		}
		switch unit {
		case 'h':
			seconds += n * 3600
		case 'm':
			seconds += n * 60
		case 's':
			seconds += n
		}
	}
	return seconds
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
