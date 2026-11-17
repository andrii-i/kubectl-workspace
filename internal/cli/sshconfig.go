/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package cli

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jupyter-infra/kubectl-workspace/internal/sshconfig"
	"github.com/jupyter-infra/kubectl-workspace/internal/tunnel"
)

type sshConfigOptions struct {
	*rootOptions
	write             bool
	remove            bool
	kubeconfigList    bool
	clusterName       string
	sshConfigFile     string
	command           string
	user              string
	caFile            string
	credentialTimeout time.Duration
	allowPlaintext    bool
}

func newSSHConfigCommand(root *rootOptions) *cobra.Command {
	o := &sshConfigOptions{rootOptions: root}
	cmd := &cobra.Command{
		Use:   "ssh-config [--write | --remove]",
		Short: "Print, install or remove the SSH configuration entry for a cluster",
		Long: `ssh-config renders the SSH configuration entry that routes every workspace alias
of one cluster, ` + sshconfig.AliasFormat + `, through
"kubectl workspace connect". Without --write it prints the entry; with --write it
installs it in ~/.ssh/config, replacing an earlier entry for the same cluster; with
--remove it deletes the entry installed for the context, or the one --cluster-name names.

The cluster name comes from the entry already installed for the context, else from
the context name or the cluster name inside a context named like an EKS cluster ARN;
--cluster-name overrides it.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return o.run()
		},
	}
	cmd.Flags().BoolVar(&o.write, writeFlag, false, "install the entry into the SSH configuration file")
	cmd.Flags().BoolVar(&o.remove, removeFlag, false,
		"remove the entry installed for the context, or the one --cluster-name names, from the SSH configuration file")
	cmd.MarkFlagsMutuallyExclusive(writeFlag, removeFlag)
	cmd.Flags().StringVar(&o.clusterName, "cluster-name", "", clusterNameHelp)
	cmd.Flags().StringVar(&o.sshConfigFile, "ssh-config-file", "", sshConfigFileHelp)
	cmd.Flags().StringVar(&o.command, "command", "",
		"path or name of the kubectl-workspace binary for the ProxyCommand (default this binary)")
	cmd.Flags().StringVar(&o.user, "user", sshconfig.DefaultUser, "SSH user name in the entry")
	cmd.Flags().StringVar(&o.caFile, "ca-file", "", caFileHelp+"; written into the entry")
	cmd.Flags().BoolVar(&o.allowPlaintext, allowPlaintextFlag, false, allowPlaintextHelp+"; written into the entry")
	cmd.Flags().DurationVar(&o.credentialTimeout, "credential-timeout", defaultCredentialTimeout,
		credentialTimeoutHelp+"; written into the entry when it differs from the default")
	return cmd
}

func (o *sshConfigOptions) run() error {
	if o.remove {
		return o.removeEntry()
	}
	entry, err := o.entry()
	if err != nil {
		return err
	}
	if !o.write {
		_, _ = fmt.Fprint(o.streams.Out, entry.Render())
		return nil
	}
	path, err := sshConfigPath(o.sshConfigFile)
	if err != nil {
		return err
	}
	warning, err := checkReplacement(path, entry, o.clusterName != "")
	if err != nil {
		return err
	}
	if err := sshconfig.Install(path, entry); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(o.streams.Out, "Installed the entry for Host %s in %s.\n", entry.HostPattern(), path)
	if warning != "" {
		warnf(o.streams.Err, "%s", warning)
	}
	o.warnAboutOtherEntries(path, entry)
	alias := strings.Replace(sshconfig.AliasFormat, "<cluster>", entry.Cluster, 1)
	_, _ = fmt.Fprintf(o.streams.Out, "Connect with: ssh %s\n", alias)
	return nil
}

// warnAboutOtherEntries names every other intact entry that routes the same cluster as
// the one just installed, with the command that removes it, since the alias under the
// old name still works; damaged entries routing the context get warnAboutDamage.
func (o *sshConfigOptions) warnAboutOtherEntries(path string, entry sshconfig.Entry) {
	warnAboutDamage(o.streams.Err, path, entry.Context)
	removeArgs := sshConfigHintArgs(removeFlag, o.sshConfigFile)
	others, _ := sshconfig.EntriesForContext(path, entry.Context, runtime.GOOS == goosWindows)
	for _, other := range others {
		if other.Damaged != nil || other.Cluster == entry.Cluster || routesAnother(other, entry.Context, entry.Kubeconfig) {
			continue
		}
		// Under a KUBECONFIG list the new entry has no pin to compare, so an entry pinned
		// to a file may well be another cluster's; its removal is not suggested.
		if o.kubeconfigList && other.Kubeconfig != "" {
			continue
		}
		hint := kubectlWorkspaceCommand("ssh-config", o.kubeconfig, entry.Context, other.Cluster, removeArgs...)
		warnf(o.streams.Err, "%s also routes context %q under the cluster name %q; drop it with: %s", path,
			entry.Context, other.Cluster, hint)
	}
}

// checkReplacement looks at the entry a --write is about to replace. Under a derived
// cluster name, an entry that routes another cluster is refused: two EKS clusters named
// alike derive the same name, and the write would silently take over the first's entry.
// With --cluster-name the replacement is the user's choice. Any replaced entry whose
// route changes, the context or the kubeconfig pin, earns a warning, since ssh then
// reaches another cluster or resolves the context in another file.
func checkReplacement(path string, entry sshconfig.Entry, named bool) (warning string, err error) {
	installed, ok, err := sshconfig.EntryFor(path, entry.Cluster, runtime.GOOS == goosWindows)
	if err != nil || !ok || installed.Context == "" {
		return "", err
	}
	theirs, ours := routePair(installed, sshconfig.InstalledEntry{Context: entry.Context, Kubeconfig: entry.Kubeconfig})
	if !named && routesAnother(installed, entry.Context, entry.Kubeconfig) {
		return "", fmt.Errorf("%s already routes %s under the cluster name %q; pass --cluster-name to put %s under "+
			"another name, or --cluster-name %s to replace that entry", path, theirs, entry.Cluster, ours, entry.Cluster)
	}
	if theirs != ours {
		return fmt.Sprintf("the entry for cluster %q routed %s and now routes %s", entry.Cluster, theirs, ours), nil
	}
	return "", nil
}

// removeEntry deletes the entry clusterToRemove names.
func (o *sshConfigOptions) removeEntry() error {
	path, err := sshConfigPath(o.sshConfigFile)
	if err != nil {
		return err
	}
	cluster, contextName, err := o.clusterToRemove(path)
	if err != nil || cluster == "" {
		return err
	}
	removed, err := sshconfig.Remove(path, cluster)
	if err != nil {
		return err
	}
	if !removed {
		_, _ = fmt.Fprintf(o.streams.Out, "No entry for cluster %q in %s.\n", cluster, path)
		return nil
	}
	host := sshconfig.Entry{Cluster: cluster}.HostPattern()
	_, _ = fmt.Fprintf(o.streams.Out, "Removed the entry for Host %s from %s.\n", host, path)
	if contextName != "" {
		warnAboutDamage(o.streams.Err, path, contextName)
	}
	return nil
}

// clusterToRemove resolves the entry --remove acts on: --cluster-name as written, since
// Remove matches the marker text and builds no alias, so an entry under a name edited by
// hand can go too; else the entry installed for the context, which it returns as well.
// With the flag the kubeconfig is not read, so an entry whose context is gone can still
// be removed. The derived default never counts: it could name another context's entry.
// The cluster is empty, after a note, when nothing routes the context; an unreadable
// file or a damaged entry routing the context is an error.
func (o *sshConfigOptions) clusterToRemove(path string) (cluster, contextName string, err error) {
	if o.clusterName != "" {
		return o.clusterName, "", nil
	}
	target, err := o.target()
	if err != nil {
		return "", "", fmt.Errorf("%w; pass --cluster-name to remove the entry without reading the kubeconfig", err)
	}
	kubeconfig, _, err := pinnedKubeconfig(o.kubeconfig)
	if err != nil {
		return "", "", err
	}
	clusters, err := installedClusters(target.Context, kubeconfig, path)
	if err != nil {
		return "", "", err
	}
	if cluster, err = pickInstalledCluster(clusters, path, target.Context); err != nil {
		return "", "", err
	}
	if cluster == "" {
		_, _ = fmt.Fprintf(o.streams.Out, "No entry in %s routes context %q; pass --cluster-name for an entry "+
			"under another name.\n", path, target.Context)
	}
	return cluster, target.Context, nil
}

// entry builds and validates the entry from the flags and the kubeconfig context.
func (o *sshConfigOptions) entry() (sshconfig.Entry, error) {
	if err := checkCredentialTimeout(o.credentialTimeout); err != nil {
		return sshconfig.Entry{}, err
	}
	target, err := o.target()
	if err != nil {
		return sshconfig.Entry{}, err
	}
	kubeconfig, err := o.kubeconfigForEntry()
	if err != nil {
		return sshconfig.Entry{}, err
	}
	cluster, err := clusterForContext(o.clusterName, target.Context, kubeconfig, o.sshConfigFile, false)
	if err != nil {
		return sshconfig.Entry{}, err
	}
	command, err := commandForEntry(o.command)
	if err != nil {
		return sshconfig.Entry{}, err
	}
	entry := sshconfig.Entry{
		Cluster:        cluster,
		Context:        target.Context,
		Kubeconfig:     kubeconfig,
		Command:        command,
		User:           o.user,
		Windows:        runtime.GOOS == goosWindows,
		AllowPlaintext: o.allowPlaintext,
	}
	if o.credentialTimeout != defaultCredentialTimeout {
		entry.CredentialTimeout = o.credentialTimeout
	}
	if o.caFile != "" {
		// Read now, so a wrong path fails here and not in every ssh session.
		if _, err := tunnel.LoadRoots(o.caFile); err != nil {
			return sshconfig.Entry{}, err
		}
		caFile, err := absPath("CA file", o.caFile)
		if err != nil {
			return sshconfig.Entry{}, err
		}
		entry.CAFile = caFile
	}
	if err := entry.Validate(); err != nil {
		return sshconfig.Entry{}, err
	}
	return entry, nil
}

// kubeconfigForEntry pins the kubeconfig file into the entry; a KUBECONFIG list
// has no single-flag form and is left to the environment.
func (o *sshConfigOptions) kubeconfigForEntry() (string, error) {
	path, list, err := pinnedKubeconfig(o.kubeconfig)
	if list {
		warnf(o.streams.Err, "KUBECONFIG lists several files; the entry relies on ssh running with the same KUBECONFIG")
	}
	o.kubeconfigList = list
	return path, err
}
