package main

import (
	"flag"
	"os"
	"syscall"

	"github.com/golang/glog"
	"github.com/openshift/machine-config-operator/cmd/common"
	"github.com/openshift/machine-config-operator/pkg/daemon"
	"github.com/openshift/machine-config-operator/pkg/version"
	"github.com/spf13/cobra"
)

var (
	startCmd = &cobra.Command{
		Use:   "start",
		Short: "Starts Machine Config Daemon",
		Long:  "",
		Run:   runStartCmd,
	}

	startOpts struct {
		kubeconfig             string
		nodeName               string
		rootMount              string
		onceFrom               string
		fromIgnition           bool
		kubeletHealthzEnabled  bool
		kubeletHealthzEndpoint string
	}
)

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.PersistentFlags().StringVar(&startOpts.kubeconfig, "kubeconfig", "", "Kubeconfig file to access a remote cluster (testing only)")
	startCmd.PersistentFlags().StringVar(&startOpts.nodeName, "node-name", "", "kubernetes node name daemon is managing.")
	startCmd.PersistentFlags().StringVar(&startOpts.rootMount, "root-mount", "/rootfs", "where the nodes root filesystem is mounted for chroot and file manipulation.")
	startCmd.PersistentFlags().StringVar(&startOpts.onceFrom, "once-from", "", "Runs the daemon once using a provided file path or URL endpoint as its machine config source")
	startCmd.PersistentFlags().BoolVar(&startOpts.fromIgnition, "from-ign", false, "Configures run-once mode to use ignition file directly instead of MachineConfig")
	startCmd.PersistentFlags().BoolVar(&startOpts.kubeletHealthzEnabled, "kubelet-healthz-enabled", true, "kubelet healthz endpoint monitoring")
	startCmd.PersistentFlags().StringVar(&startOpts.kubeletHealthzEndpoint, "kubelet-healthz-endpoint", "http://localhost:10248/healthz", "healthz endpoint to check health")
}

func runStartCmd(cmd *cobra.Command, args []string) {
	flag.Set("logtostderr", "true")
	flag.Parse()

	glog.V(2).Infof("Options parsed: %+v", startOpts)

	// To help debugging, immediately log version
	glog.Infof("Version: %+v", version.Version)

	operatingSystem, err := daemon.GetHostRunningOS(startOpts.rootMount)
	if err != nil {
		glog.Fatalf("Error found when checking operating system: %s", err)
	}

	if startOpts.nodeName == "" {
		name, ok := os.LookupEnv("NODE_NAME")
		if !ok || name == "" {
			glog.Fatalf("node-name is required")
		}
		startOpts.nodeName = name
	}

	// Ensure that the rootMount exists
	if _, err := os.Stat(startOpts.rootMount); err != nil {
		if os.IsNotExist(err) {
			glog.Fatalf("rootMount %s does not exist", startOpts.rootMount)
		}
		glog.Fatalf("unable to verify rootMount %s exists: %s", startOpts.rootMount, err)
	}
	stopCh := make(chan struct{})
	defer close(stopCh)
	var dn *daemon.Daemon
	var ctx *common.ControllerContext

	glog.Info("starting node writer")
	nodeWriter := daemon.NewNodeWriter()
	go nodeWriter.Run(stopCh)

	// If we are asked to run once and it's a valid file system path use
	// the bare Daemon
	if startOpts.onceFrom != "" {
		dn, err = daemon.New(
			startOpts.rootMount,
			startOpts.nodeName,
			operatingSystem,
			daemon.NewNodeUpdaterClient(),
			daemon.NewFileSystemClient(),
			startOpts.onceFrom,
			startOpts.fromIgnition,
			startOpts.kubeletHealthzEnabled,
			startOpts.kubeletHealthzEndpoint,
			nodeWriter,
		)
		if err != nil {
			glog.Fatalf("failed to initialize single run daemon: %v", err)
		}
		// Else we use the cluster driven daemon
	} else {
		cb, err := common.NewClientBuilder(startOpts.kubeconfig)
		if err != nil {
			glog.Fatalf("failed to initialize daemon: %v", err)
		}
		ctx = common.CreateControllerContext(cb, stopCh, componentName)
		// create the daemon instance. this also initializes kube client items
		// which need to come from the container and not the chroot.
		dn, err = daemon.NewClusterDrivenDaemon(
			startOpts.rootMount,
			startOpts.nodeName,
			operatingSystem,
			daemon.NewNodeUpdaterClient(),
			cb.MachineConfigClientOrDie(componentName),
			cb.KubeClientOrDie(componentName),
			daemon.NewFileSystemClient(),
			startOpts.onceFrom,
			startOpts.fromIgnition,
			ctx.KubeInformerFactory.Core().V1().Nodes(),
			startOpts.kubeletHealthzEnabled,
			startOpts.kubeletHealthzEndpoint,
			nodeWriter,
		)
		if err != nil {
			glog.Fatalf("failed to initialize daemon: %v", err)
		}
	}

	glog.Infof(`Calling chroot("%s")`, startOpts.rootMount)
	if err := syscall.Chroot(startOpts.rootMount); err != nil {
		glog.Fatalf("unable to chroot to %s: %s", startOpts.rootMount, err)
	}

	glog.V(2).Infof("Moving to / inside the chroot")
	if err := os.Chdir("/"); err != nil {
		glog.Fatalf("unable to change directory to /: %s", err)
	}

	if startOpts.onceFrom == "" {
		err = dn.CheckStateOnBoot(stopCh)
		if err != nil {
			glog.Fatalf("error checking initial state of node: %v", err)
		}
		ctx.KubeInformerFactory.Start(ctx.Stop)
		close(ctx.KubeInformersStarted)
	}

	glog.Info("Starting MachineConfigDaemon")
	defer glog.Info("Shutting down MachineConfigDaemon")

	err = dn.Run(stopCh)
	if err != nil {
		glog.Fatalf("failed to run: %v", err)
	}
}
