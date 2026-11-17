/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package cli

import (
	"fmt"
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
// context and kubeconfig in the SSH configuration file, else the label clusterName
// derives from the context name.
func clusterForContext(flag, contextName, kubeconfig, sshConfigFile string) (string, error) {
	if flag != "" {
		return clusterName(flag, contextName)
	}
	cluster, err := installedCluster(contextName, kubeconfig, sshConfigFile)
	if err != nil || cluster != "" {
		return cluster, err
	}
	return clusterName("", contextName)
}

// installedCluster returns the cluster name of the entry installed for the context and
// kubeconfig, empty when there is none. Two entries that match need --cluster-name to
// tell them apart, and a name edited into a marker by hand has to be a label. A file
// that cannot be read counts as holding no entry; the command that needs the file
// reports it.
func installedCluster(contextName, kubeconfig, sshConfigFile string) (string, error) {
	clusters, path := installedClusters(contextName, kubeconfig, sshConfigFile)
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

// installedClusters lists the cluster names of the entries installed for the context,
// sorted, and the file they were read from; none when the file cannot be read. An
// entry pinned to another kubeconfig file routes another cluster of the same context
// name and does not count; entries with the same pin, or none on either side, win
// over entries pinned on one side only.
func installedClusters(contextName, kubeconfig, sshConfigFile string) (clusters []string, path string) {
	path, err := sshConfigPath(sshConfigFile)
	if err != nil {
		return nil, ""
	}
	entries, err := sshconfig.EntriesForContext(path, contextName, runtime.GOOS == goosWindows)
	if err != nil {
		return nil, path
	}
	var same, oneSided []string
	for _, e := range entries {
		switch {
		case e.Kubeconfig == kubeconfig:
			same = append(same, e.Cluster)
		case e.Kubeconfig == "" || kubeconfig == "":
			oneSided = append(oneSided, e.Cluster)
		}
	}
	clusters = oneSided
	if len(same) > 0 {
		clusters = same
	}
	sort.Strings(clusters)
	return clusters, path
}

// routesAnother reports whether an installed entry routes another cluster than the
// context and kubeconfig at hand: another context name, or the same name pinned to
// another kubeconfig file; an unpinned side matches any file. An entry whose
// ProxyCommand the plugin cannot read is not judged.
func routesAnother(installed sshconfig.InstalledEntry, contextName, kubeconfig string) bool {
	if installed.Context == "" {
		return false
	}
	return installed.Context != contextName ||
		(installed.Kubeconfig != "" && kubeconfig != "" && installed.Kubeconfig != kubeconfig)
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
	if a.Kubeconfig == b.Kubeconfig {
		a.Kubeconfig, b.Kubeconfig = "", ""
	}
	return routeOf(a), routeOf(b)
}

// pinnedKubeconfig returns the kubeconfig path an entry carries for the flag: the
// --kubeconfig value, else a single-file KUBECONFIG, made absolute because OpenSSH
// runs the ProxyCommand from its own working directory; empty for the default
// lookup. list reports a KUBECONFIG with several files, which the entry leaves to
// the environment.
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
// always names the cluster: the label is otherwise read from the installed entries,
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

// clusterName resolves the alias label: the --cluster-name flag when given, else
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
