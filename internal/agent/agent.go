package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	agentconfig "github.com/YangKeao/tiproxy-machine-operator/internal/agent/config"
	"github.com/YangKeao/tiproxy-machine-operator/internal/agent/docker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Agent struct {
	Client       client.Client
	Docker       *docker.Runner
	HTTPClient   *http.Client
	Namespace    string
	MachineGroup string
	MachineID    string
	SyncPeriod   time.Duration
	DefaultImage string

	lastConfigHash string
}

func (a *Agent) Run(ctx context.Context) error {
	if a.SyncPeriod <= 0 {
		a.SyncPeriod = 10 * time.Second
	}
	if a.Docker == nil {
		a.Docker = docker.NewRunner("")
	}
	if a.HTTPClient == nil {
		a.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	if a.Namespace == "" {
		a.Namespace = "default"
	}

	logger := ctrl.LoggerFrom(ctx).WithValues("machineGroup", a.MachineGroup, "machineID", a.MachineID)
	runOnce := func() {
		if err := a.syncOnce(ctx); err != nil {
			logger.Error(err, "sync failed")
		}
	}
	runOnce()

	ticker := time.NewTicker(a.SyncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.removeMachineStatus(cleanupCtx); err != nil {
				logger.Error(err, "best-effort machine status cleanup failed")
			}
			cancel()
			return nil
		case <-ticker.C:
			runOnce()
		}
	}
}

func (a *Agent) syncOnce(ctx context.Context) error {
	if a.MachineGroup == "" {
		return fmt.Errorf("machine group is required")
	}
	if a.MachineID == "" {
		return fmt.Errorf("machine id is required")
	}

	mg := &tiproxyv1alpha1.TiProxyMachineGroup{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.MachineGroup}, mg); err != nil {
		return err
	}
	settings := a.runtimeSettings(mg)

	cfgBytes, err := agentconfig.BuildTiProxyConfig(agentconfig.RenderOptions{
		ResolvedLinks:     mg.Status.ResolvedLinks,
		APIPort:           settings.apiPort,
		ListenHost:        settings.listenHost,
		PortRangeStart:    mg.Spec.PortRange.Start,
		PortRangeEnd:      mg.Spec.PortRange.End,
		DefaultListenPort: mg.Spec.PortRange.Start,
	})
	if err != nil {
		_ = a.upsertMachineStatus(ctx, tiproxyv1alpha1.MachinePhaseFailed, settings.image, "", err.Error())
		return err
	}
	cfgHash := agentconfig.HashBytes(cfgBytes)
	if err := writeConfigFile(settings.configPath, cfgBytes); err != nil {
		_ = a.upsertMachineStatus(ctx, tiproxyv1alpha1.MachinePhaseFailed, settings.image, cfgHash, err.Error())
		return err
	}

	running, err := a.Docker.IsRunning(ctx, settings.containerName)
	if err != nil {
		_ = a.upsertMachineStatus(ctx, tiproxyv1alpha1.MachinePhaseFailed, settings.image, cfgHash, err.Error())
		return err
	}

	justStarted := false
	if !running {
		err = a.Docker.Start(ctx, docker.StartOptions{
			Name:       settings.containerName,
			Image:      settings.image,
			ConfigPath: settings.configPath,
			CertDir:    settings.certDir,
			ExtraArgs:  settings.extraArgs,
		})
		if err != nil {
			_ = a.upsertMachineStatus(ctx, tiproxyv1alpha1.MachinePhaseFailed, settings.image, cfgHash, err.Error())
			return err
		}
		justStarted = true
	}

	if !justStarted && cfgHash != a.lastConfigHash {
		if err := a.pushConfig(ctx, settings.apiBaseURL, cfgBytes); err != nil {
			_ = a.upsertMachineStatus(ctx, tiproxyv1alpha1.MachinePhaseFailed, settings.image, cfgHash, err.Error())
			return err
		}
	}

	a.lastConfigHash = cfgHash
	phase := tiproxyv1alpha1.MachinePhaseRunning
	message := "tiproxy is running"
	if justStarted {
		phase = tiproxyv1alpha1.MachinePhaseStarting
		message = "tiproxy container started with rendered config"
	}
	return a.upsertMachineStatus(ctx, phase, settings.image, cfgHash, message)
}

func (a *Agent) pushConfig(ctx context.Context, apiBaseURL string, cfg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(apiBaseURL, "/")+"/api/admin/config", bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/toml")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("config api status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (a *Agent) upsertMachineStatus(ctx context.Context, phase, image, cfgHash, message string) error {
	key := types.NamespacedName{Namespace: a.Namespace, Name: a.MachineGroup}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mg := &tiproxyv1alpha1.TiProxyMachineGroup{}
		if err := a.Client.Get(ctx, key, mg); err != nil {
			return err
		}
		statusBase := mg.DeepCopy()
		if mg.Status.Machines == nil {
			mg.Status.Machines = map[string]tiproxyv1alpha1.MachineStatus{}
		}
		mg.Status.Machines[a.MachineID] = tiproxyv1alpha1.MachineStatus{
			ObservedGeneration: mg.Generation,
			LastHeartbeatTime:  metav1.Now(),
			Phase:              phase,
			ConfigHash:         cfgHash,
			TiProxyImage:       image,
			Message:            message,
		}
		return a.Client.Status().Patch(ctx, mg, client.MergeFrom(statusBase))
	})
}

func (a *Agent) removeMachineStatus(ctx context.Context) error {
	if a.MachineGroup == "" || a.MachineID == "" {
		return nil
	}
	key := types.NamespacedName{Namespace: a.Namespace, Name: a.MachineGroup}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mg := &tiproxyv1alpha1.TiProxyMachineGroup{}
		if err := a.Client.Get(ctx, key, mg); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if mg.Status.Machines == nil {
			return nil
		}
		if _, ok := mg.Status.Machines[a.MachineID]; !ok {
			return nil
		}
		statusBase := mg.DeepCopy()
		delete(mg.Status.Machines, a.MachineID)
		if len(mg.Status.Machines) == 0 {
			mg.Status.Machines = nil
		}
		return a.Client.Status().Patch(ctx, mg, client.MergeFrom(statusBase))
	})
}

type runtimeSettings struct {
	image         string
	containerName string
	configPath    string
	certDir       string
	listenHost    string
	apiPort       int32
	apiBaseURL    string
	extraArgs     []string
}

func (a *Agent) runtimeSettings(mg *tiproxyv1alpha1.TiProxyMachineGroup) runtimeSettings {
	resolvedImage := resolveTiProxyImage(mg.Spec.TiProxy, a.DefaultImage)
	s := runtimeSettings{
		image:         resolvedImage,
		containerName: mg.Spec.TiProxy.ContainerName,
		configPath:    mg.Spec.TiProxy.ConfigPath,
		certDir:       mg.Spec.TiProxy.CertDir,
		listenHost:    "0.0.0.0",
		apiPort:       mg.Spec.TiProxy.APIPort,
		extraArgs:     slices.Clone(mg.Spec.TiProxy.ExtraArgs),
	}
	if s.image == "" {
		s.image = a.DefaultImage
	}
	if s.containerName == "" {
		s.containerName = "tiproxy"
	}
	if s.configPath == "" {
		s.configPath = "/var/lib/tiproxy-machine-agent/tiproxy.toml"
	}
	if s.apiPort == 0 {
		s.apiPort = 3080
	}
	scheme := mg.Spec.TiProxy.APIScheme
	if scheme == "" {
		scheme = "http"
	}
	s.apiBaseURL = fmt.Sprintf("%s://127.0.0.1:%d", scheme, s.apiPort)
	return s
}

func resolveTiProxyImage(spec tiproxyv1alpha1.TiProxyRuntimeSpec, fallback string) string {
	if image := strings.TrimSpace(spec.Image); image != "" {
		return image
	}

	base := strings.TrimSpace(spec.BaseImage)
	version := strings.TrimSpace(spec.Version)
	if base == "" && version == "" {
		return fallback
	}
	if base == "" {
		base = imageBase(fallback)
	}
	if version == "" {
		return base
	}
	return base + ":" + version
}

func imageBase(image string) string {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return "pingcap/tiproxy"
	}
	lastSlash := strings.LastIndex(trimmed, "/")
	lastColon := strings.LastIndex(trimmed, ":")
	if lastColon > lastSlash {
		return trimmed[:lastColon]
	}
	return trimmed
}

func writeConfigFile(path string, content []byte) error {
	if path == "" {
		return fmt.Errorf("empty config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, content) {
		return nil
	}
	return os.WriteFile(path, content, 0o644)
}
