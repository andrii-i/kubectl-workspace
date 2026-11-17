/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/jupyter-infra/kubectl-workspace/internal/sshconfig"
)

// commandForEntry resolves --command for the ProxyCommand line: this binary when
// empty, a path made absolute because OpenSSH runs the command from its own working
// directory, and a bare name left to the PATH lookup at connection time.
func commandForEntry(command string) (string, error) {
	switch {
	case command == "":
		return executablePath()
	case strings.ContainsAny(command, `/\`):
		return absPath("command", command)
	default:
		return command, nil
	}
}

// executablePath returns the path OpenSSH should run. When this binary is on PATH
// under its own name, that path is preferred: krew's PATH entry is a symlink into
// a versioned store directory, and the resolved path would break at the next
// upgrade.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to locate this binary: %w", err)
	}
	if onPath, err := exec.LookPath(filepath.Base(exe)); err == nil && sameFile(onPath, exe) {
		return onPath, nil
	}
	return exe, nil
}

// clusterForContext resolves the cluster name in the alias for a context: the
// --cluster-name flag when given, else the name of the entry already installed for the
// context and kubeconfig in the SSH configuration file, else the cluster name clusterName
// derives from the context name. A file that cannot be read, or a damaged entry routing
// the context, is an error when strict and counts as no entry otherwise: printing needs
// no readable file, and a write reports the damage after acting on the file. Two
// matching entries, or a name edited into a marker by hand, are errors on every path,
// since the name derived instead could be another cluster's.
func clusterForContext(flag, contextName, kubeconfig, sshConfigFile string, strict bool) (string, error) {
	if flag != "" {
		return clusterName(flag, contextName)
	}
	var clusters []string
	path, err := sshConfigPath(sshConfigFile)
	if err == nil {
		clusters, err = installedClusters(contextName, kubeconfig, path)
	}
	if err != nil && strict {
		return "", err
	}
	cluster, err := pickInstalledCluster(clusters, path, contextName)
	if err != nil || cluster != "" {
		return cluster, err
	}
	return clusterName("", contextName)
}

// pickInstalledCluster returns the one cluster name among the entries installed for the
// context, empty when there is none. Two entries need --cluster-name to tell them
// apart, and a name edited into a marker by hand has to be a label.
func pickInstalledCluster(clusters []string, path, contextName string) (string, error) {
	switch len(clusters) {
	case 0:
		return "", nil
	case 1:
		if err := sshconfig.ValidateClusterName(clusters[0]); err != nil {
			return "", fmt.Errorf("%s has an entry for context %q under %q, which cannot be the cluster name "+
				"in the alias (%w); pass --cluster-name", path, contextName, clusters[0], err)
		}
		return clusters[0], nil
	}
	return "", fmt.Errorf("%s has entries for context %q under the cluster names %s; pass --cluster-name",
		path, contextName, strings.Join(clusters, ", "))
}

// installedClusters lists the cluster names of the intact entries installed for the
// context in the file at path, sorted. An entry pinned to another kubeconfig file routes
// another cluster of the same context name and does not count; entries with the same
// pin, or none on either side, win over entries pinned on one side only. The error
// reports a file that cannot be read, or a damaged entry routing the context when no
// intact one does.
func installedClusters(contextName, kubeconfig, path string) ([]string, error) {
	entries, err := sshconfig.EntriesForContext(path, contextName, runtime.GOOS == goosWindows)
	if err != nil {
		return nil, err
	}
	var same, oneSided []string
	var damaged error
	for _, e := range entries {
		switch {
		case e.Damaged != nil:
			if damaged == nil {
				damaged = fmt.Errorf("%s: %w", path, e.Damaged)
			}
		case samePin(e.Kubeconfig, kubeconfig):
			same = append(same, e.Cluster)
		case e.Kubeconfig == "" || kubeconfig == "":
			oneSided = append(oneSided, e.Cluster)
		}
	}
	clusters := oneSided
	if len(same) > 0 {
		clusters = same
	}
	if len(clusters) == 0 {
		return nil, damaged
	}
	sort.Strings(clusters)
	return clusters, nil
}

// warnAboutDamage reports each distinct damage among the entries routing the context,
// once: OpenSSH still honours the Host line of a block whose markers are broken, and
// only a hand can repair it.
func warnAboutDamage(w io.Writer, path, contextName string) {
	entries, _ := sshconfig.EntriesForContext(path, contextName, runtime.GOOS == goosWindows)
	said := map[string]bool{}
	for _, e := range entries {
		if e.Damaged == nil || said[e.Damaged.Error()] {
			continue
		}
		said[e.Damaged.Error()] = true
		warnf(w, "%s: %v", path, e.Damaged)
	}
}

// routesAnother reports whether an installed entry routes another cluster than the
// context and kubeconfig at hand: another context name, or the same name pinned to
// another kubeconfig file; an unpinned side matches any file. An entry whose
// ProxyCommand the plugin cannot read is not judged here; the callers refuse it.
func routesAnother(installed sshconfig.InstalledEntry, contextName, kubeconfig string) bool {
	if installed.Context == "" {
		return false
	}
	return installed.Context != contextName ||
		(installed.Kubeconfig != "" && kubeconfig != "" && !samePin(installed.Kubeconfig, kubeconfig))
}

// samePin reports whether two kubeconfig pins name the same file: the same spelling,
// or two spellings of one file, as a dotfiles symlink or a case difference on a
// case-insensitive filesystem gives. An empty pin matches only an empty pin.
func samePin(a, b string) bool {
	return a == b || (a != "" && b != "" && sameFile(a, b))
}

// routeOf names what an entry routes, for messages.
func routeOf(e sshconfig.InstalledEntry) string {
	if e.Kubeconfig == "" {
		return fmt.Sprintf("context %q", e.Context)
	}
	return fmt.Sprintf("context %q of kubeconfig %s", e.Context, e.Kubeconfig)
}

// routePair names what two entries route, for a message that contrasts them: the
// kubeconfig file appears only when it tells them apart.
func routePair(a, b sshconfig.InstalledEntry) (string, string) {
	if samePin(a.Kubeconfig, b.Kubeconfig) {
		a.Kubeconfig, b.Kubeconfig = "", ""
	}
	return routeOf(a), routeOf(b)
}

// pinnedKubeconfig returns the kubeconfig pin of an entry, the kubeconfig path its
// ProxyCommand carries, for the flag: the --kubeconfig value, else a single-file
// KUBECONFIG, made absolute because OpenSSH runs the ProxyCommand from its own working
// directory; empty for the default lookup. list reports a KUBECONFIG with several
// files, which the entry leaves to the environment.
func pinnedKubeconfig(flag string) (path string, list bool, err error) {
	env := os.Getenv("KUBECONFIG")
	switch {
	case flag != "":
		path, err = absPath("kubeconfig", flag)
	case env == "":
	case strings.Contains(env, string(os.PathListSeparator)):
		list = true
	default:
		path, err = absPath("kubeconfig", env)
	}
	return path, list, err
}

// sshConfigHintArgs returns the flags of a pasted ssh-config hint: the action flag,
// and the configuration file when it is not the default, so the pasted command acts
// on the same file.
func sshConfigHintArgs(action, sshConfigFile string) []string {
	args := []string{"--" + action}
	if sshConfigFile != "" {
		args = append(args, "--ssh-config-file", sshConfigFile)
	}
	return args
}

// kubectlWorkspaceCommand renders a command the user can paste into a shell. It
// always names the cluster: the cluster name is otherwise read from the installed entries,
// and the pasted command has to act on the cluster it was built for whatever entries
// exist by then.
func kubectlWorkspaceCommand(subcommand, kubeconfig, contextName, cluster string, args ...string) string {
	parts := []string{"kubectl", "workspace", subcommand}
	parts = append(parts, args...)
	if kubeconfig != "" {
		parts = append(parts, "--kubeconfig", kubeconfig)
	}
	parts = append(parts, "--context", contextName, "--cluster-name", cluster)
	quote := sshconfig.ShellQuote
	if runtime.GOOS == goosWindows {
		quote = sshconfig.WindowsQuote
	}
	for i, part := range parts {
		parts[i] = quote(part)
	}
	return strings.Join(parts, " ")
}

// clusterName resolves the cluster name in the alias: the --cluster-name flag when given, else
// the context name, or for a context named like an EKS cluster ARN the cluster name
// inside it, lowercased; either has to be a valid label.
func clusterName(flag, contextName string) (string, error) {
	if flag != "" {
		return flag, sshconfig.ValidateClusterName(flag)
	}
	candidate := contextName
	if name, ok := eksClusterName(contextName); ok {
		candidate = name
	}
	// OpenSSH lowercases host names before matching, and ParseAlias lowercases too.
	candidate = strings.ToLower(candidate)
	if err := sshconfig.ValidateClusterName(candidate); err != nil {
		return "", fmt.Errorf("%q cannot be the cluster name in the alias (%w); pass --cluster-name", candidate, err)
	}
	return candidate, nil
}

// eksClusterARN matches the context name `aws eks update-kubeconfig` creates by
// default, arn:<partition>:eks:<region>:<account>:cluster/<name>.
var eksClusterARN = regexp.MustCompile(`^arn:[^:]+:eks:[^:]*:[^:]*:cluster/([^/]+)$`)

// eksClusterName returns the cluster name of a context named like an EKS cluster ARN.
func eksClusterName(contextName string) (string, bool) {
	m := eksClusterARN.FindStringSubmatch(contextName)
	if m == nil {
		return "", false
	}
	return m[1], true
}
