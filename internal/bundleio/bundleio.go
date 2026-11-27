// Package bundleio reads and writes coldstart's portable, redacted bundle
// archives. A bundle is a gzip-compressed tar archive whose bundle.json member
// contains the normalized model.Bundle.
package bundleio

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

const (
	bundleJSONName = "bundle.json"

	// These limits bound both memory use and the work performed while reading an
	// untrusted archive. They are deliberately well above normal coldstart
	// bundles, which are usually only a few megabytes.
	maxCompressedArchiveSize   = 32 << 20
	maxUncompressedArchiveSize = 64 << 20
	maxArchiveEntries          = 256
	maxArchiveMemberSize       = 32 << 20
	maxBundleJSONSize          = 16 << 20
)

type archiveEntry struct {
	name string
	data []byte
}

// Write writes b as a gzip-compressed tar archive. The caller's bundle is not
// modified: a normalized copy is redacted and its logs are trimmed before any
// output is produced.
func Write(w io.Writer, b model.Bundle) error {
	if w == nil {
		return errors.New("bundleio: nil writer")
	}

	entries, err := prepareEntries(b)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	modTime := b.CapturedAt
	if modTime.IsZero() {
		modTime = time.Unix(0, 0).UTC()
	}
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     0o600,
			Size:     int64(len(entry.data)),
			Typeflag: tar.TypeReg,
			ModTime:  modTime,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("bundleio: write tar header %q: %w", entry.name, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("bundleio: write tar member %q: %w", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("bundleio: close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("bundleio: close gzip writer: %w", err)
	}
	return nil
}

func prepareEntries(b model.Bundle) ([]archiveEntry, error) {
	redacted, err := normalizeAndRedact(b)
	if err != nil {
		return nil, fmt.Errorf("bundleio: redact bundle: %w", err)
	}

	bundleJSON, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bundleio: encode bundle.json: %w", err)
	}
	bundleJSON = append(bundleJSON, '\n')

	podYAML, err := marshalYAML(redacted.Pod)
	if err != nil {
		return nil, fmt.Errorf("bundleio: encode pod.yaml: %w", err)
	}

	events := redacted.Events
	if events == nil {
		events = []corev1.Event{}
	}
	eventsYAML, err := marshalYAML(events)
	if err != nil {
		return nil, fmt.Errorf("bundleio: encode events.yaml: %w", err)
	}

	entries := []archiveEntry{
		{name: bundleJSONName, data: bundleJSON},
		{name: "pod.yaml", data: podYAML},
	}
	if redacted.Node != nil {
		nodeYAML, err := marshalYAML(redacted.Node)
		if err != nil {
			return nil, fmt.Errorf("bundleio: encode node.yaml: %w", err)
		}
		entries = append(entries, archiveEntry{name: "node.yaml", data: nodeYAML})
	}
	entries = append(entries, archiveEntry{name: "events.yaml", data: eventsYAML})

	logEntries, err := makeLogEntries(redacted.Logs)
	if err != nil {
		return nil, err
	}
	return append(entries, logEntries...), nil
}

// marshalYAML goes through JSON so Kubernetes' json field names and custom JSON
// encodings (notably quantities and timestamps) are retained in the YAML.
func marshalYAML(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return yaml.Marshal(value)
}

func safeContainerName(name string) string {
	var cleaned strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(name) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_'
		if valid {
			cleaned.WriteRune(r)
			lastSeparator = r == '.' || r == '-' || r == '_'
			continue
		}
		if !lastSeparator {
			cleaned.WriteByte('-')
			lastSeparator = true
		}
	}
	result := strings.Trim(cleaned.String(), ".-_")
	if len(result) > 96 {
		result = strings.Trim(result[:96], ".-_")
	}
	if result == "" || result == "." || result == ".." {
		return "container"
	}
	return result
}

func makeLogEntries(logs []model.LogCapture) ([]archiveEntry, error) {
	entries := make([]archiveEntry, 0, len(logs))
	used := make(map[string]struct{}, len(logs))
	for _, capture := range logs {
		base := safeContainerName(capture.Container)
		if capture.Previous {
			base += "-previous"
		}
		name := "logs/" + base + ".log"
		for suffix := 2; ; suffix++ {
			if _, exists := used[name]; !exists {
				break
			}
			name = fmt.Sprintf("logs/%s-%d.log", base, suffix)
		}
		used[name] = struct{}{}

		var data strings.Builder
		for _, line := range capture.Lines {
			data.WriteString(line.Timestamp.Format(timestampLayout))
			data.WriteByte('\t')
			data.WriteString(line.Text)
			data.WriteByte('\n')
		}
		entries = append(entries, archiveEntry{name: name, data: []byte(data.String())})
	}
	return entries, nil
}

// Read reads and validates a gzip tar bundle, returning the model.Bundle stored
// in bundle.json. Every archive member is validated even though only bundle.json
// is decoded, so unsafe paths, links, duplicate metadata, and corrupt input are
// rejected rather than silently ignored.
func Read(r io.Reader) (model.Bundle, error) {
	if r == nil {
		return model.Bundle{}, errors.New("bundleio: nil reader")
	}

	compressed, err := io.ReadAll(io.LimitReader(r, maxCompressedArchiveSize+1))
	if err != nil {
		return model.Bundle{}, fmt.Errorf("bundleio: read compressed archive: %w", err)
	}
	if len(compressed) > maxCompressedArchiveSize {
		return model.Bundle{}, fmt.Errorf("bundleio: compressed archive exceeds %d bytes", maxCompressedArchiveSize)
	}

	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return model.Bundle{}, fmt.Errorf("bundleio: open gzip stream: %w", err)
	}
	archive, readErr := io.ReadAll(io.LimitReader(gz, maxUncompressedArchiveSize+1))
	closeErr := gz.Close()
	if readErr != nil {
		return model.Bundle{}, fmt.Errorf("bundleio: decompress archive: %w", readErr)
	}
	if closeErr != nil {
		return model.Bundle{}, fmt.Errorf("bundleio: close gzip stream: %w", closeErr)
	}
	if len(archive) > maxUncompressedArchiveSize {
		return model.Bundle{}, fmt.Errorf("bundleio: uncompressed archive exceeds %d bytes", maxUncompressedArchiveSize)
	}

	reader := bytes.NewReader(archive)
	tr := tar.NewReader(reader)
	seen := make(map[string]struct{}, maxArchiveEntries)
	var bundleJSON []byte
	entryCount := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return model.Bundle{}, fmt.Errorf("bundleio: read tar header: %w", err)
		}
		entryCount++
		if entryCount > maxArchiveEntries {
			return model.Bundle{}, fmt.Errorf("bundleio: archive has more than %d entries", maxArchiveEntries)
		}
		memberName := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			memberName = strings.TrimSuffix(hdr.Name, "/")
			if hdr.Size != 0 {
				return model.Bundle{}, fmt.Errorf("bundleio: directory %q has a non-zero size", hdr.Name)
			}
		}
		if !validArchivePath(memberName) {
			return model.Bundle{}, fmt.Errorf("bundleio: unsafe archive path %q", hdr.Name)
		}
		if _, exists := seen[memberName]; exists {
			return model.Bundle{}, fmt.Errorf("bundleio: duplicate archive member %q", memberName)
		}
		seen[memberName] = struct{}{}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA && hdr.Typeflag != tar.TypeDir {
			return model.Bundle{}, fmt.Errorf("bundleio: unsupported archive member type %d for %q", hdr.Typeflag, hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if memberName == bundleJSONName {
				return model.Bundle{}, errors.New("bundleio: bundle.json is not a regular file")
			}
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxArchiveMemberSize {
			return model.Bundle{}, fmt.Errorf("bundleio: archive member %q exceeds %d bytes", hdr.Name, maxArchiveMemberSize)
		}

		data, err := io.ReadAll(io.LimitReader(tr, maxArchiveMemberSize+1))
		if err != nil {
			return model.Bundle{}, fmt.Errorf("bundleio: read archive member %q: %w", hdr.Name, err)
		}
		if int64(len(data)) != hdr.Size {
			return model.Bundle{}, fmt.Errorf("bundleio: truncated archive member %q", hdr.Name)
		}
		if hdr.Name == bundleJSONName {
			if len(data) > maxBundleJSONSize {
				return model.Bundle{}, fmt.Errorf("bundleio: bundle.json exceeds %d bytes", maxBundleJSONSize)
			}
			bundleJSON = data
		}
	}

	// A tar end marker terminates the archive. Non-zero data after it is not a
	// valid member and usually indicates a malformed or smuggled archive.
	if remaining := archive[len(archive)-reader.Len():]; hasNonZeroBytes(remaining) {
		return model.Bundle{}, errors.New("bundleio: non-zero data after tar end marker")
	}
	if bundleJSON == nil {
		return model.Bundle{}, errors.New("bundleio: archive is missing bundle.json")
	}

	var bundle model.Bundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return model.Bundle{}, fmt.Errorf("bundleio: decode bundle.json: %w", err)
	}
	if bundle.Schema != model.BundleSchema {
		return model.Bundle{}, fmt.Errorf("bundleio: unsupported bundle schema %q (want %q)", bundle.Schema, model.BundleSchema)
	}
	return bundle, nil
}

func validArchivePath(name string) bool {
	if name == "" || name == "." || name == ".." || path.IsAbs(name) {
		return false
	}
	if strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	// Reject Windows drive-qualified paths even when reading on Unix.
	if len(name) >= 2 && name[1] == ':' && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func hasNonZeroBytes(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return true
		}
	}
	return false
}
