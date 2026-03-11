package main

import (
	"flag"
	"os"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/agent"
	"github.com/YangKeao/tiproxy-machine-operator/internal/agent/docker"
	"github.com/YangKeao/tiproxy-machine-operator/internal/agent/imds"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	var (
		namespace    string
		machineGroup string
		machineID    string
		kubeconfig   string
		syncPeriod   time.Duration
		defaultImage string
		dockerHost   string
	)

	flag.StringVar(&namespace, "namespace", getenv("TIPROXY_MACHINE_NAMESPACE", "default"), "Namespace of TiProxyMachineGroup.")
	flag.StringVar(&machineGroup, "machine-group", getenv("TIPROXY_MACHINE_GROUP", ""), "Name of TiProxyMachineGroup.")
	flag.StringVar(&machineID, "machine-id", getenv("TIPROXY_MACHINE_ID", ""), "Stable machine identifier. Defaults to AWS instance-id or hostname.")
	kubeconfig = registerKubeconfigFlag(getenv("TIPROXY_KUBECONFIG", ""))
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Second, "Sync period.")
	flag.StringVar(&defaultImage, "default-image", getenv("TIPROXY_DEFAULT_IMAGE", "pingcap/tiproxy:latest"), "Fallback TiProxy image.")
	flag.StringVar(&dockerHost, "docker-host", getenv("TIPROXY_DOCKER_HOST", ""), "Docker daemon host (optional, defaults to environment and local socket).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if flag.Lookup("kubeconfig") != nil {
		kubeconfig = flag.Lookup("kubeconfig").Value.String()
	}

	if machineGroup == "" {
		klog.Error("missing required flag: --machine-group")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	ctx := ctrl.SetupSignalHandler()
	machineID = imds.ResolveMachineID(ctx, machineID)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = tiproxyv1alpha1.AddToScheme(scheme)

	cfg, err := buildKubeConfig(kubeconfig)
	if err != nil {
		klog.ErrorS(err, "failed to get kube config")
		os.Exit(1)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "failed to create kube client")
		os.Exit(1)
	}

	a := &agent.Agent{
		Client:       cl,
		Scheme:       scheme,
		Docker:       docker.NewRunner(dockerHost),
		Namespace:    namespace,
		MachineGroup: machineGroup,
		MachineID:    machineID,
		SyncPeriod:   syncPeriod,
		DefaultImage: defaultImage,
	}
	if err := a.Run(ctx); err != nil {
		klog.ErrorS(err, "agent exits with error")
		os.Exit(1)
	}
}

func buildKubeConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}

func registerKubeconfigFlag(defaultValue string) string {
	if f := flag.Lookup("kubeconfig"); f != nil {
		if defaultValue != "" {
			_ = flag.Set("kubeconfig", defaultValue)
		}
		return f.Value.String()
	}
	flag.String("kubeconfig", defaultValue, "Path to kubeconfig for out-of-cluster API access.")
	return defaultValue
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
