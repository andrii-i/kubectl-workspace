/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// newFileMode keeps a new configuration file private, as OpenSSH requires.
const newFileMode = 0o600

// DefaultPath returns the user's SSH configuration file, ~/.ssh/config.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to find the home directory: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// Install writes the entry into the SSH configuration file at path, creating the
// file when needed. An existing entry for the same cluster is replaced in place; a
// new one goes where firstBlock says.
func Install(path string, e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	path, content, mode, err := load(path)
	if err != nil {
		return err
	}
	updated, err := merge(content, e)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return write(path, updated, mode)
}

// EntryFor returns the entry installed for cluster in the file at path, and whether
// there is one. windows selects the quoting the entry was written with.
func EntryFor(path, cluster string, windows bool) (InstalledEntry, bool, error) {
	path, content, _, err := load(path)
	if err != nil {
		return InstalledEntry{}, false, err
	}
	lines := splitLines(content)
	start, end, err := findEntry(lines, cluster)
	if err != nil {
		return InstalledEntry{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if start < 0 {
		return InstalledEntry{}, false, nil
	}
	return readEntry(cluster, lines[start:end+1], windows), true, nil
}

// InstalledEntry is what EntriesForContext reads back from an entry: its cluster label and
// the context and kubeconfig file its ProxyCommand runs connect with, the kubeconfig
// empty when connect uses the default lookup.
type InstalledEntry struct {
	Cluster    string
	Context    string
	Kubeconfig string
	// Damaged is the marker error of an entry whose markers do not pair up; its flags
	// were read as far as the next marker. Nil for an intact entry.
	Damaged error
}

// EntriesForContext returns the entries in the file at path whose ProxyCommand runs
// connect with contextName, in file order. windows selects the quoting the entries
// were written with. An entry with broken markers comes back with Damaged set, so a
// lookup can skip it and a command about to act on the file can report it.
func EntriesForContext(path, contextName string, windows bool) ([]InstalledEntry, error) {
	_, content, _, err := load(path)
	if err != nil {
		return nil, err
	}
	lines := splitLines(content)
	var entries []InstalledEntry
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		var cluster string
		var block []string
		var damaged error
		switch {
		case strings.HasPrefix(trimmed, startMarkerPrefix):
			cluster = strings.TrimPrefix(trimmed, startMarkerPrefix)
			start, end, err := findEntry(lines, cluster)
			if err != nil {
				start, end, damaged = i, nextMarker(lines, i+1)-1, err
				// With the end marker directly above, the entry's lines sit between the two.
				if p := prevMarker(lines, i-1); p >= 0 && strings.TrimSpace(lines[p]) == endMarker(cluster) {
					start = p + 1
				}
			}
			block = lines[start : end+1]
			i = end
		case strings.HasPrefix(trimmed, endMarkerPrefix):
			// An end marker of a cluster with a start marker was passed over with its
			// block above; one without is the remains of an entry whose start was lost.
			cluster = strings.TrimPrefix(trimmed, endMarkerPrefix)
			if hasMarker(lines, startMarker(cluster)) {
				continue
			}
			_, _, damaged = findEntry(lines, cluster)
			block = lines[prevMarker(lines, i-1)+1 : i+1]
		default:
			continue
		}
		entry := readEntry(cluster, block, windows)
		entry.Damaged = damaged
		if entry.Context == contextName {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// hasMarker reports whether a line is the marker.
func hasMarker(lines []string, marker string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

// isMarker reports whether the line is a start or an end marker of any cluster.
func isMarker(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, startMarkerPrefix) || strings.HasPrefix(trimmed, endMarkerPrefix)
}

// nextMarker returns the index of the first marker line at or after from, or the
// number of lines when there is none.
func nextMarker(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if isMarker(lines[i]) {
			return i
		}
	}
	return len(lines)
}

// prevMarker returns the index of the last marker line at or before from, or -1 when
// there is none.
func prevMarker(lines []string, from int) int {
	for i := from; i >= 0; i-- {
		if isMarker(lines[i]) {
			return i
		}
	}
	return -1
}

// readEntry reads the connect flags out of an entry's ProxyCommand line.
func readEntry(cluster string, block []string, windows bool) InstalledEntry {
	entry := InstalledEntry{Cluster: cluster}
	for _, line := range block {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ProxyCommand ")
		if !ok {
			continue
		}
		args := SplitArgs(rest, windows)
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "--context":
				entry.Context = args[i+1]
			case "--kubeconfig":
				entry.Kubeconfig = args[i+1]
			}
		}
	}
	return entry
}

// Remove deletes the entry for cluster from the file at path and reports whether
// there was one. Of the blank lines Install put around the entry, one goes with it,
// so the file reads as it did before Install.
func Remove(path, cluster string) (bool, error) {
	path, content, mode, err := load(path)
	if err != nil {
		return false, err
	}
	lines := splitLines(content)
	start, end, err := findEntry(lines, cluster)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if start < 0 {
		return false, nil
	}
	out := make([]string, 0, len(lines)-(end-start+1))
	out = append(out, lines[:start]...)
	out = append(out, lines[end+1:]...)
	switch {
	case start < len(out) && out[start] == "" && (start == 0 || out[start-1] == ""):
		out = append(out[:start], out[start+1:]...)
	case start == len(out) && start > 0 && out[start-1] == "":
		out = out[:start-1]
	}
	if len(out) == 0 {
		return true, write(path, "", mode)
	}
	newline := lineEnding(content)
	return true, write(path, strings.Join(out, newline)+newline, mode)
}

// lineEnding returns the line ending the content uses, so a rewrite keeps it.
func lineEnding(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// load follows a symlinked path and reads the file: the path read, its content and
// its mode.
func load(path string) (resolved, content string, mode os.FileMode, err error) {
	if resolved, err = resolveTarget(path); err != nil {
		return "", "", 0, err
	}
	if content, mode, err = read(resolved); err != nil {
		return "", "", 0, err
	}
	return resolved, content, mode, nil
}

// resolveTarget follows a symlinked configuration file, which dotfiles setups
// have, so the rename in write replaces the file OpenSSH reads and not the link.
func resolveTarget(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		// A link whose target is missing resolves the same way as no file; renaming
		// over it would replace the link with a regular file.
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a link to a missing file; create the target or remove the link", path)
		}
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to resolve %s: %w", path, err)
	}
	return resolved, nil
}

func read(path string) (string, os.FileMode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", newFileMode, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("failed to read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	return string(data), info.Mode().Perm(), nil
}

// write replaces the file through a rename so a crash leaves either version whole.
func write(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to set the mode of %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

// merge returns the configuration text with the entry in place.
func merge(content string, e Entry) (string, error) {
	newline := lineEnding(content)
	lines := splitLines(content)
	entry := splitLines(e.Render())

	start, end, err := findEntry(lines, e.Cluster)
	if err != nil {
		return "", err
	}

	var out []string
	if start >= 0 {
		out = append(out, lines[:start]...)
		out = append(out, entry...)
		out = append(out, lines[end+1:]...)
		return strings.Join(out, newline) + newline, nil
	}
	at := firstBlock(lines)
	if at < 0 {
		at = len(lines)
	}
	before, after := lines[:at], lines[at:]
	out = append(out, before...)
	if len(before) > 0 && before[len(before)-1] != "" {
		out = append(out, "")
	}
	out = append(out, entry...)
	if len(after) > 0 {
		out = append(out, "")
	}
	out = append(out, after...)
	return strings.Join(out, newline) + newline, nil
}

// splitLines splits on either line ending and drops the empty element a trailing
// newline produces, so a rejoin does not grow the file by one line per run.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// findEntry returns the line range of the entry for cluster, or -1 when absent. A
// start marker without its end, an end marker without its start, or a second marker
// of either kind is an error: the file needs a hand before the tool touches it.
func findEntry(lines []string, cluster string) (start, end int, err error) {
	start, end = -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case startMarker(cluster):
			if start >= 0 {
				return -1, -1, fmt.Errorf("two start markers for cluster %q; remove the entries by hand", cluster)
			}
			start = i
		case endMarker(cluster):
			switch {
			case start < 0:
				return -1, -1, fmt.Errorf("an end marker for cluster %q precedes its start; remove it by hand", cluster)
			case end >= 0:
				return -1, -1, fmt.Errorf("two end markers for cluster %q; remove the entries by hand", cluster)
			}
			end = i
		}
	}
	if start >= 0 && end < 0 {
		return -1, -1, fmt.Errorf("the entry for cluster %q has no end marker; remove it by hand", cluster)
	}
	return start, end, nil
}

// blockKeyword matches the first line of a Host or Match block. ssh_config
// accepts `Keyword value` and `Keyword=value`, in any letter case.
var blockKeyword = regexp.MustCompile(`(?i)^\s*(host|match)(\s|=)`)

// firstBlock returns the index where a new entry goes: the first Host or Match line
// with the comment lines directly above it, or -1 when the file has no block. The
// placement rule is in AGENT.md under Invariants; another cluster's entry counts
// from its start marker, so a new entry lands before it, not inside its markers.
func firstBlock(lines []string) int {
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), startMarkerPrefix) {
			return i
		}
		if blockKeyword.MatchString(line) {
			for i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
				i--
			}
			return i
		}
	}
	return -1
}
