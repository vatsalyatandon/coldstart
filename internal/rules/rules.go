// Package rules loads versioned engine rule packs and matches startup log
// markers. Rule packs are data, not code: custom packs can be supplied without
// rebuilding coldstart.
package rules

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/vatsalyatandon/coldstart/internal/model"
	rulesdata "github.com/vatsalyatandon/coldstart/rules"
)

type Rule struct {
	Start        string `yaml:"start"`
	End          string `yaml:"end"`
	DurationFrom string `yaml:"duration_from"`
	startRE      *regexp.Regexp
	endRE        *regexp.Regexp
}

type AttrRule struct {
	Pattern    string  `yaml:"pattern"`
	Multiplier float64 `yaml:"multiplier"`
	re         *regexp.Regexp
}

type Recommendation struct {
	Text       string `yaml:"text"`
	Confidence string `yaml:"confidence"`
}

type Pack struct {
	Engine          string                    `yaml:"engine"`
	Versions        string                    `yaml:"versions"`
	Detect          []string                  `yaml:"detect"`
	Phases          map[string]Rule           `yaml:"phases"`
	Flags           map[string]string         `yaml:"flags"`
	Attrs           map[string]AttrRule       `yaml:"attrs"`
	Recommendations map[string]Recommendation `yaml:"recommendations"`

	Path       string              `json:"-"`
	Constraint *semver.Constraints `json:"-"`
	custom     bool
	detectRE   []*regexp.Regexp
	flagRE     map[string]*regexp.Regexp
}

type Detection struct {
	Engine    string
	Version   string
	Container string
	FirstLog  *time.Time
}

type Selection struct {
	Pack    Pack
	Warning string
}

type PhaseMatch struct {
	Start        *time.Time
	End          *time.Time
	ReportedSecs *float64
}

type MatchResult struct {
	Phases map[string]PhaseMatch
	Flags  map[string]bool
	Attrs  map[string]float64
}

// Load reads embedded packs and then any YAML packs in customDir. Custom packs
// are considered first when multiple packs have the same version constraint.
func Load(customDir string) ([]Pack, error) {
	var packs []Pack
	err := fs.WalkDir(rulesdata.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		data, err := fs.ReadFile(rulesdata.FS(), path)
		if err != nil {
			return err
		}
		pack, err := parsePack(data, "embedded/"+path)
		if err != nil {
			return err
		}
		packs = append(packs, pack)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load embedded rules: %w", err)
	}

	if customDir != "" {
		err := filepath.WalkDir(customDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
				return nil
			}
			data, err := os.ReadFile(path) // #nosec G304 -- custom rule directories are explicit user input.
			if err != nil {
				return err
			}
			pack, err := parsePack(data, path)
			if err != nil {
				return err
			}
			pack.custom = true
			packs = append([]Pack{pack}, packs...)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read custom rules: %w", err)
		}
	}

	sort.SliceStable(packs, func(i, j int) bool {
		if packs[i].Engine != packs[j].Engine {
			return packs[i].Engine < packs[j].Engine
		}
		if packs[i].custom != packs[j].custom {
			return packs[i].custom
		}
		return packRank(packs[i]) < packRank(packs[j])
	})
	return packs, nil
}

func parsePack(data []byte, path string) (Pack, error) {
	var pack Pack
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&pack); err != nil {
		return Pack{}, fmt.Errorf("%s: %w", path, err)
	}
	pack.Engine = strings.ToLower(strings.TrimSpace(pack.Engine))
	if pack.Engine == "" {
		return Pack{}, fmt.Errorf("%s: engine is required", path)
	}
	pack.Path = path
	if pack.Versions == "" {
		pack.Constraint = nil
	} else {
		constraint, err := semver.NewConstraint(pack.Versions)
		if err != nil {
			return Pack{}, fmt.Errorf("%s: invalid versions %q: %w", path, pack.Versions, err)
		}
		pack.Constraint = constraint
	}
	for i, pattern := range pack.Detect {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return Pack{}, fmt.Errorf("%s: detect[%d]: %w", path, i, err)
		}
		pack.detectRE = append(pack.detectRE, re)
	}
	if len(pack.detectRE) == 0 {
		return Pack{}, fmt.Errorf("%s: at least one detect pattern is required", path)
	}
	for id, rule := range pack.Phases {
		if rule.Start != "" {
			re, err := regexp.Compile(rule.Start)
			if err != nil {
				return Pack{}, fmt.Errorf("%s: phases.%s.start: %w", path, id, err)
			}
			rule.startRE = re
		}
		if rule.End != "" {
			re, err := regexp.Compile(rule.End)
			if err != nil {
				return Pack{}, fmt.Errorf("%s: phases.%s.end: %w", path, id, err)
			}
			rule.endRE = re
		}
		if rule.startRE == nil && rule.endRE == nil {
			return Pack{}, fmt.Errorf("%s: phases.%s needs start or end", path, id)
		}
		pack.Phases[id] = rule
	}
	pack.flagRE = make(map[string]*regexp.Regexp)
	for name, pattern := range pack.Flags {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return Pack{}, fmt.Errorf("%s: flags.%s: %w", path, name, err)
		}
		pack.flagRE[name] = re
	}
	for name, attr := range pack.Attrs {
		re, err := regexp.Compile(attr.Pattern)
		if err != nil {
			return Pack{}, fmt.Errorf("%s: attrs.%s: %w", path, name, err)
		}
		attr.re = re
		pack.Attrs[name] = attr
	}
	return pack, nil
}

// Detect identifies an engine from its log signature, falling back to pod
// container metadata when logs are unavailable.
func Detect(bundle model.Bundle, packs []Pack) Detection {
	var best Detection
	bestTime := time.Time{}
	for _, capture := range bundle.Logs {
		for _, line := range capture.Lines {
			for _, pack := range packs {
				for _, re := range pack.detectRE {
					match := re.FindStringSubmatch(line.Text)
					if match == nil {
						continue
					}
					if best.Engine == "" || line.Timestamp.Before(bestTime) || (line.Timestamp.Equal(bestTime) && capture.Container < best.Container) {
						best = Detection{Engine: pack.Engine, Container: capture.Container, Version: namedValue(match, re, "version"), FirstLog: timePtr(line.Timestamp)}
						bestTime = line.Timestamp
					}
				}
			}
		}
	}
	if best.Engine != "" {
		return best
	}

	for _, container := range bundle.Pod.Spec.Containers {
		haystack := strings.ToLower(container.Name + " " + container.Image + " " + strings.Join(container.Args, " ") + " " + strings.Join(container.Command, " "))
		engine := ""
		switch {
		case strings.Contains(haystack, "vllm"):
			engine = "vllm"
		case strings.Contains(haystack, "sglang"):
			engine = "sglang"
		}
		if engine == "" {
			continue
		}
		version := versionFromText(strings.Join(container.Args, " ") + " " + strings.Join(container.Command, " "))
		if version == "" {
			for _, env := range container.Env {
				if strings.Contains(strings.ToLower(env.Name), "version") && env.Value != "" {
					version = versionFromText(env.Value)
				}
			}
		}
		return Detection{Engine: engine, Version: version, Container: container.Name}
	}
	return Detection{}
}

// Select picks an exact pack or the nearest pack for the same engine. The
// fallback is always surfaced as a warning.
func Select(packs []Pack, detection Detection) (Selection, bool) {
	candidates := make([]Pack, 0, len(packs))
	for _, pack := range packs {
		if pack.Engine == detection.Engine {
			candidates = append(candidates, pack)
		}
	}
	if len(candidates) == 0 {
		return Selection{}, false
	}
	version, err := semver.NewVersion(coerceVersion(detection.Version))
	if detection.Version == "" || err != nil {
		pack := candidates[len(candidates)-1]
		for _, candidate := range candidates {
			if candidate.custom {
				pack = candidate
				break
			}
		}
		warning := fmt.Sprintf("engine version not detected; used %s rules", pack.Path)
		if detection.Version != "" {
			warning = fmt.Sprintf("could not parse %s version %q; used %s rules", detection.Engine, detection.Version, pack.Path)
		}
		return Selection{Pack: pack, Warning: warning}, true
	}
	for _, pack := range candidates {
		if pack.Constraint == nil || pack.Constraint.Check(version) {
			return Selection{Pack: pack}, true
		}
	}

	nearest := candidates[0]
	nearestDistance := versionDistance(version, nearest)
	for _, pack := range candidates[1:] {
		if distance := versionDistance(version, pack); distance < nearestDistance {
			nearest, nearestDistance = pack, distance
		}
	}
	warning := fmt.Sprintf("⚠ rule pack for %s %s not found; used %s rules", detection.Engine, detection.Version, nearest.Path)
	return Selection{Pack: nearest, Warning: warning}, true
}

// Match returns the strongest marker for each phase. When tensor-parallel
// workers each report a duration, the largest reported duration wins.
func Match(lines []model.LogLine, pack Pack) MatchResult {
	result := MatchResult{Phases: make(map[string]PhaseMatch), Flags: make(map[string]bool), Attrs: make(map[string]float64)}
	ids := make([]string, 0, len(pack.Phases))
	for id := range pack.Phases {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, line := range lines {
		for name, re := range pack.flagRE {
			if re.MatchString(line.Text) {
				result.Flags[name] = true
			}
		}
		for name, attr := range pack.Attrs {
			match := attr.re.FindStringSubmatch(line.Text)
			if match == nil {
				continue
			}
			value, err := strconv.ParseFloat(namedValue(match, attr.re, "value"), 64)
			if err == nil {
				value *= attr.Multiplier
				if value > result.Attrs[name] {
					result.Attrs[name] = value
				}
			}
		}
	}

	for _, id := range ids {
		rule := pack.Phases[id]
		var starts []model.LogLine
		var ends []endCandidate
		for _, line := range lines {
			if line.Timestamp.IsZero() {
				continue
			}
			if rule.startRE != nil && rule.startRE.MatchString(line.Text) {
				starts = append(starts, line)
			}
			if rule.endRE != nil {
				if match := rule.endRE.FindStringSubmatch(line.Text); match != nil {
					var secs *float64
					if rule.DurationFrom != "" {
						value, err := strconv.ParseFloat(firstNamedValue(match, rule.endRE, rule.DurationFrom), 64)
						if err == nil {
							secs = &value
						}
					}
					ends = append(ends, endCandidate{line: line, secs: secs})
				}
			}
		}
		if len(ends) == 0 {
			continue
		}
		best := ends[0]
		for _, candidate := range ends[1:] {
			if betterEnd(candidate, best) {
				best = candidate
			}
		}
		end := timePtr(best.line.Timestamp)
		var start *time.Time
		for _, candidate := range starts {
			if candidate.Timestamp.After(best.line.Timestamp) {
				continue
			}
			if start == nil || candidate.Timestamp.Before(*start) {
				start = timePtr(candidate.Timestamp)
			}
		}
		if start == nil && best.secs != nil {
			candidate := best.line.Timestamp.Add(-time.Duration(*best.secs * float64(time.Second)))
			start = &candidate
		}
		if start == nil {
			// End-only rules are point markers (for example server_ready).
			start = end
		}
		if end.Before(*start) {
			continue
		}
		result.Phases[id] = PhaseMatch{Start: start, End: end, ReportedSecs: best.secs}
	}
	return result
}

type endCandidate struct {
	line model.LogLine
	secs *float64
}

func betterEnd(candidate, current endCandidate) bool {
	if candidate.secs != nil && current.secs != nil && *candidate.secs != *current.secs {
		return *candidate.secs > *current.secs
	}
	if candidate.secs != nil && current.secs == nil {
		return true
	}
	return candidate.line.Timestamp.After(current.line.Timestamp)
}

func namedValue(match []string, re *regexp.Regexp, name string) string {
	index := re.SubexpIndex(name)
	if index >= 0 && index < len(match) {
		return match[index]
	}
	return ""
}

func firstNamedValue(match []string, re *regexp.Regexp, name string) string {
	if value := namedValue(match, re, name); value != "" {
		return value
	}
	return namedValue(match, re, name+"2")
}

func versionFromText(text string) string {
	re := regexp.MustCompile(`(?i)(?:version|vllm|sglang)[^\d]{0,20}(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.-]+)?)`)
	if match := re.FindStringSubmatch(text); len(match) > 1 {
		return match[1]
	}
	return ""
}

func coerceVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "0.0.0"
	}
	parts := strings.Split(version, ".")
	if len(parts) == 1 {
		return version + ".0.0"
	}
	if len(parts) == 2 {
		return version + ".0"
	}
	return version
}

func packRank(pack Pack) float64 {
	if pack.Constraint == nil {
		return 0
	}
	// Constraints do not expose their lower bound. Use the first numeric token
	// in the textual constraint only for deterministic ordering; selection is
	// always performed by semver.Constraint.Check.
	re := regexp.MustCompile(`\d+(?:\.\d+){0,2}`)
	if match := re.FindString(pack.Versions); match != "" {
		if version, err := semver.NewVersion(coerceVersion(match)); err == nil {
			return float64(version.Major()*1000000 + version.Minor()*1000 + version.Patch())
		}
	}
	return 0
}

func versionDistance(version *semver.Version, pack Pack) float64 {
	lower := lowerVersion(pack)
	if lower == nil {
		return 0
	}
	distance := abs(versionRank(version) - versionRank(lower))
	if lower.GreaterThan(version) {
		// Prefer the newest pack at or below the observed version. A newer pack
		// is only a fallback when no older compatible pack exists.
		return 1e12 + distance
	}
	return distance
}

func lowerVersion(pack Pack) *semver.Version {
	if pack.Constraint == nil {
		return nil
	}
	re := regexp.MustCompile(`\d+(?:\.\d+){0,2}`)
	if match := re.FindString(pack.Versions); match != "" {
		if version, err := semver.NewVersion(coerceVersion(match)); err == nil {
			return version
		}
	}
	return nil
}

func versionRank(version *semver.Version) float64 {
	return float64(version.Major()*1000000 + version.Minor()*1000 + version.Patch())
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func timePtr(value time.Time) *time.Time {
	return &value
}
