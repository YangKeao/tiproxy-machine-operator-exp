package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud"
	cloudaws "github.com/YangKeao/tiproxy-machine-operator/internal/cloud/aws"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud/noop"
	"github.com/YangKeao/tiproxy-machine-operator/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var (
		metricsAddr          string
		healthProbeAddr      string
		enableLeaderElection bool
		cloudProvider        string
		awsRegion            string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&cloudProvider, "cloud-provider", "noop", "Cloud provider implementation: noop or aws.")
	flag.StringVar(&awsRegion, "aws-region", "", "AWS region override when cloud-provider=aws.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	scheme := runtimeScheme()

	mgr, err := ctrl.NewManager(restConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "tiproxy-machine-operator-controller",
	})
	if err != nil {
		klog.ErrorS(err, "unable to start manager")
		os.Exit(1)
	}

	cloudManager, err := buildCloudManager(context.Background(), cloudProvider, awsRegion)
	if err != nil {
		klog.ErrorS(err, "unable to build cloud manager")
		os.Exit(1)
	}

	if err := (&controller.MachineGroupReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		CloudManager: cloudManager,
		Provider:     cloudProvider,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create controller", "controller", "TiProxyMachineGroup")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.ErrorS(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.ErrorS(err, "unable to set up ready check")
		os.Exit(1)
	}

	klog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.ErrorS(err, "problem running manager")
		os.Exit(1)
	}
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = tiproxyv1alpha1.AddToScheme(scheme)
	return scheme
}

func restConfigOrDie() *rest.Config {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		panic(fmt.Sprintf("get kubernetes rest config: %v", err))
	}
	return cfg
}

func buildCloudManager(ctx context.Context, provider, region string) (cloud.Manager, error) {
	switch provider {
	case "aws":
		return cloudaws.NewManager(ctx, region)
	case "noop", "":
		return noop.NewManager(), nil
	default:
		return nil, fmt.Errorf("unknown cloud provider %q", provider)
	}
}
