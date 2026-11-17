/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jupyter-infra/kubectl-workspace/internal/testutil"
)

func TestInstallCreatesThePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ssh", "config")
	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != testEntry().Render() {
		t.Errorf("new file =\n%s\nwant the rendered entry alone", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// OpenSSH refuses a configuration file others can write.
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Dir(path), 0o700)
}

func TestInstallWritesThroughASymlinkedFile(t *testing.T) {
	target := writeFile(t, "Host github.com\n  User git\n")
	link := filepath.Join(t.TempDir(), "config")
	testutil.Symlink(t, target, link)

	if err := Install(link, testEntry()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	want := testEntry().Render() + "\nHost github.com\n  User git\n"
	if got := readFile(t, target); got != want {
		t.Errorf("target file =\n%s\nwant\n%s", got, want)
	}
	if _, ok, err := EntryFor(link, "prod", false); err != nil || !ok {
		t.Errorf("EntryFor through the link = %v, %v", ok, err)
	}
}

func TestInstallPlacesTheEntry(t *testing.T) {
	rendered := testEntry().Render()
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{
			name:     "appended after the options when there is no Host block",
			existing: "ServerAliveInterval 60\n",
			want:     "ServerAliveInterval 60\n\n" + rendered,
		},
		{
			name:     "before the first Host block, after an Include",
			existing: "Include ~/.ssh/conf.d/*\n\nHost github.com\n  User git\n\nHost *\n  ServerAliveInterval 60\n",
			want: "Include ~/.ssh/conf.d/*\n\n" + rendered +
				"\nHost github.com\n  User git\n\nHost *\n  ServerAliveInterval 60\n",
		},
		{
			name:     "before a Match block",
			existing: "Match host bastion\n  User ops\n",
			want:     rendered + "\nMatch host bastion\n  User ops\n",
		},
		{
			name:     "before a lowercase keyword=value block",
			existing: "host=github.com\n  User git\n",
			want:     rendered + "\nhost=github.com\n  User git\n",
		},
		{
			name:     "separated from the preceding line by a blank line",
			existing: "Include ~/.ssh/conf.d/*\nHost github.com\n  User git\n",
			want:     "Include ~/.ssh/conf.d/*\n\n" + rendered + "\nHost github.com\n  User git\n",
		},
		{
			name:     "above the comment that belongs to the first block",
			existing: "Include ~/.ssh/conf.d/*\n# GitHub\nHost github.com\n  User git\n",
			want:     "Include ~/.ssh/conf.d/*\n\n" + rendered + "\n# GitHub\nHost github.com\n  User git\n",
		},
		{
			name:     "a file without a trailing newline",
			existing: "Host github.com\n  User git",
			want:     rendered + "\nHost github.com\n  User git\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, tt.existing)
			if err := Install(path, testEntry()); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path); got != tt.want {
				t.Errorf("file =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestInstallRefusesALinkToAMissingFile(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config")
	testutil.Symlink(t, filepath.Join(dir, "dotfiles", "ssh_config"), link)

	err := Install(link, testEntry())
	if err == nil || !strings.Contains(err.Error(), "link to a missing file") {
		t.Errorf("Install error = %v, want the dangling link named", err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v, %v", info, err)
	}
}

func TestInstallKeepsEachClusterInsideItsOwnMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	staging, prod := testEntry(), testEntry()
	staging.Cluster, prod.Cluster = "staging", "prod"
	for _, e := range []Entry{staging, prod, staging} {
		if err := Install(path, e); err != nil {
			t.Fatal(err)
		}
	}

	// A new cluster lands before the existing entry; the reinstall replaces staging in place.
	want := prod.Render() + "\n" + staging.Render()
	if got := readFile(t, path); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestInstallKeepsTheFilesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are Unix")
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("Host github.com\n  User git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o644)
}

func TestInstallLeavesTheFileAloneWhenTheDirectoryIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs Unix permissions enforced for a non-root user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("Host github.com\n  User git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := Install(path, testEntry())
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("Install error = %v, want it to name the directory %s", err, dir)
	}
	if got := readFile(t, path); got != "Host github.com\n  User git\n" {
		t.Errorf("a failed install changed the file:\n%s", got)
	}
}

func TestInstallReplacesTheEntryForTheSameCluster(t *testing.T) {
	old := testEntry()
	old.Command = "/old/path/kubectl-workspace"
	other := testEntry()
	other.Cluster = "staging"
	other.Context = "staging"
	existing := "Host github.com\n  User git\n\n" + old.Render() + "\n" + other.Render() + "\nHost *\n  ServerAliveInterval 60\n"
	path := writeFile(t, existing)

	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, path)
	want := "Host github.com\n  User git\n\n" + testEntry().Render() + "\n" + other.Render() + "\nHost *\n  ServerAliveInterval 60\n"
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if strings.Count(got, startMarker("prod")) != 1 {
		t.Errorf("expected exactly one entry for prod, got %d", strings.Count(got, startMarker("prod")))
	}
}

func TestInstallKeepsCRLF(t *testing.T) {
	path := writeFile(t, "Host github.com\r\n  User git\r\n")
	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("expected every line ending to be CRLF, got\n%q", got)
	}
	if !strings.HasPrefix(got, startMarker("prod")+"\r\nHost *.prod.jk8s\r\n") {
		t.Errorf("entry not written with CRLF:\n%q", got)
	}
}

func TestInstallRejectsAPartialEntry(t *testing.T) {
	path := writeFile(t, startMarker("prod")+"\nHost *.prod.jk8s\n  User jovyan\n")
	err := Install(path, testEntry())
	if err == nil || !strings.Contains(err.Error(), "remove it by hand") {
		t.Fatalf("Install error = %v, want a request to repair the file by hand", err)
	}
	if got := readFile(t, path); !strings.HasPrefix(got, startMarker("prod")) {
		t.Errorf("a failed install must leave the file untouched, got\n%s", got)
	}
}

func TestEntryFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if _, ok, err := EntryFor(path, "prod", false); err != nil || ok {
		t.Fatalf("EntryFor on a missing file = %v, %v; want false, nil", ok, err)
	}
	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := EntryFor(path, "prod", false)
	if err != nil || !ok || entry.Context != testEntry().Context || entry.Kubeconfig != testEntry().Kubeconfig {
		t.Errorf("EntryFor after Install = %+v, %v, %v; want the installed entry's context and kubeconfig", entry, ok, err)
	}
	if _, ok, err := EntryFor(path, "staging", false); err != nil || ok {
		t.Errorf("EntryFor for another cluster = %v, %v; want false, nil", ok, err)
	}
}

func TestEntryForRejectsAPartialEntry(t *testing.T) {
	path := writeFile(t, endMarker("prod")+"\n")
	if _, _, err := EntryFor(path, "prod", false); err == nil || !strings.Contains(err.Error(), "remove it by hand") {
		t.Errorf("EntryFor error = %v, want a request to repair the file by hand", err)
	}
}

// A block pasted twice, or a stray marker after a complete block, is a damaged file:
// the lookup marks it and the writes refuse it, instead of acting on the first copy.
func TestDuplicatedMarkersAreDamage(t *testing.T) {
	rendered := testEntry().Render()
	tests := map[string]string{
		"the block twice":            rendered + rendered,
		"a stray start marker after": rendered + startMarker("prod") + "\n",
		"a stray end marker after":   rendered + endMarker("prod") + "\n",
		"the start marker lost":      strings.TrimPrefix(rendered, startMarker("prod")+"\n"),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, content)
			entries, err := EntriesForContext(path, "prod", false)
			if err != nil || len(entries) == 0 {
				t.Fatalf("entries = %v, %v; want the entry returned, marked damaged", entries, err)
			}
			for _, e := range entries {
				if e.Damaged == nil || e.Cluster != "prod" {
					t.Errorf("entry %+v; want cluster prod marked damaged", e)
				}
			}
			if _, err := Remove(path, "prod"); err == nil || !strings.Contains(err.Error(), "by hand") {
				t.Errorf("Remove error = %v, want a request to repair the file by hand", err)
			}
			if err := Install(path, testEntry()); err == nil || !strings.Contains(err.Error(), "by hand") {
				t.Errorf("Install error = %v, want a request to repair the file by hand", err)
			}
			if got := readFile(t, path); got != content {
				t.Errorf("file changed to\n%s\nwant it untouched", got)
			}
		})
	}
}

func TestDefaultPathIsTheUsersSSHConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".ssh", "config"); got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %o, want %o", path, got, want)
	}
}

func TestEntriesForContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if entries, err := EntriesForContext(path, "prod", false); err != nil || len(entries) != 0 {
		t.Fatalf("missing file: entries = %v, %v; want none, nil", entries, err)
	}
	arn := "arn:aws:eks:us-west-2:123456789012:cluster/dev"
	for _, e := range []Entry{
		testEntry(),
		{Cluster: "dev", Context: arn, Kubeconfig: "/My Files/kubeconfig", Command: "/My Tools/kubectl-workspace", User: DefaultUser},
		{Cluster: "dev-two", Context: arn, Command: "/opt/kw", User: DefaultUser},
	} {
		if err := Install(path, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		context string
		want    []InstalledEntry
	}{
		{"prod", []InstalledEntry{{Cluster: "prod", Context: "prod"}}},
		// Install puts a newer entry before the older one.
		{arn, []InstalledEntry{{Cluster: "dev-two", Context: arn}, {Cluster: "dev", Context: arn, Kubeconfig: "/My Files/kubeconfig"}}},
		{"staging", nil},
	} {
		entries, err := EntriesForContext(path, tt.context, false)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(entries) != fmt.Sprint(tt.want) {
			t.Errorf("entries for %q = %v, want %v", tt.context, entries, tt.want)
		}
	}
}

// A damaged entry comes back marked, with the flags read as far as the next marker, so
// a lookup can leave it out and a command about to act on the file can report it.
func TestEntriesForContextMarksADamagedEntry(t *testing.T) {
	path := writeFile(t, startMarker("broken")+"\n  ProxyCommand kw connect --stdio --context prod %h\n")
	if err := Install(path, testEntry()); err != nil {
		t.Fatal(err)
	}
	entries, err := EntriesForContext(path, "prod", false)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %v, %v; want the damaged and the intact entry", entries, err)
	}
	byCluster := map[string]InstalledEntry{}
	for _, e := range entries {
		byCluster[e.Cluster] = e
	}
	if e := byCluster["broken"]; e.Damaged == nil || !strings.Contains(e.Damaged.Error(), "no end marker") {
		t.Errorf("broken entry = %+v; want it marked damaged for its missing end marker", e)
	}
	if e := byCluster["prod"]; e.Context != "prod" || e.Damaged != nil {
		t.Errorf("prod entry = %+v; want the intact entry", e)
	}
}

// Remove undoes Install: the file ends up as it was, except that one blank line may
// separate what the entry had split, since Install padded both sides of the entry.
func TestRemoveUndoesInstall(t *testing.T) {
	tests := []struct {
		name, before, after string
	}{
		{name: "empty file", before: "", after: ""},
		{name: "only a block", before: "Host github.com\n  User git\n", after: "Host github.com\n  User git\n"},
		{name: "options then a block", before: "AddKeysToAgent yes\nHost github.com\n", after: "AddKeysToAgent yes\n\nHost github.com\n"},
		{name: "options only", before: "AddKeysToAgent yes\n", after: "AddKeysToAgent yes\n"},
		{name: "crlf", before: "Host a\r\n  User x\r\n", after: "Host a\r\n  User x\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, tt.before)
			if err := Install(path, testEntry()); err != nil {
				t.Fatal(err)
			}
			removed, err := Remove(path, "prod")
			if err != nil || !removed {
				t.Fatalf("Remove = %v, %v; want true, nil", removed, err)
			}
			if got := readFile(t, path); got != tt.after {
				t.Errorf("after Remove:\n%q\nwant\n%q", got, tt.after)
			}
		})
	}
}

func TestRemoveLeavesOtherClustersAndReportsAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	staging := testEntry()
	staging.Cluster, staging.Context = "staging", "staging"
	for _, e := range []Entry{testEntry(), staging} {
		if err := Install(path, e); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := Remove(path, "prod"); err != nil || !removed {
		t.Fatalf("Remove prod = %v, %v", removed, err)
	}
	if got := readFile(t, path); got != staging.Render() {
		t.Errorf("after removing prod:\n%s\nwant the staging entry alone", got)
	}
	if removed, err := Remove(path, "prod"); err != nil || removed {
		t.Errorf("second Remove = %v, %v; want false, nil", removed, err)
	}
	if removed, err := Remove(filepath.Join(t.TempDir(), "none"), "prod"); err != nil || removed {
		t.Errorf("Remove on a missing file = %v, %v; want false, nil", removed, err)
	}
}

func TestRemoveRejectsAPartialEntry(t *testing.T) {
	path := writeFile(t, endMarker("prod")+"\n")
	if _, err := Remove(path, "prod"); err == nil || !strings.Contains(err.Error(), "remove it by hand") {
		t.Errorf("error = %v, want a request to repair the file by hand", err)
	}
}

func TestRemoveWritesThroughASymlinkedFile(t *testing.T) {
	target := writeFile(t, "Host github.com\n  User git\n")
	link := filepath.Join(t.TempDir(), "config")
	testutil.Symlink(t, target, link)
	if err := Install(link, testEntry()); err != nil {
		t.Fatal(err)
	}

	if removed, err := Remove(link, "prod"); err != nil || !removed {
		t.Fatalf("Remove through the link = %v, %v; want true, nil", removed, err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if got := readFile(t, target); got != "Host github.com\n  User git\n" {
		t.Errorf("target file =\n%s\nwant the content before Install", got)
	}
}
