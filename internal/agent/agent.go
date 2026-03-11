package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	agentconfig "github.com/YangKeao/tiproxy-machine-operator/internal/agent/config"
	"github.com/YangKeao/tiproxy-machine-operator/internal/agent/docker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Agent struct {
	Client       client.Client
	Scheme       *runtime.Scheme
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
		_ = a.upsertMachineStatus(ctx, mg, tiproxyv1alpha1.TiProxyMachinePhaseFailed, settings.image, "", err.Error())
		return err
	}
	cfgHash := agentconfig.HashBytes(cfgBytes)
	if err := writeConfigFile(settings.configPath, cfgBytes); err != nil {
		_ = a.upsertMachineStatus(ctx, mg, tiproxyv1alpha1.TiProxyMachinePhaseFailed, settings.image, cfgHash, err.Error())
		return err
	}

	running, err := a.Docker.IsRunning(ctx, settings.containerName)
	if err != nil {
		_ = a.upsertMachineStatus(ctx, mg, tiproxyv1alpha1.TiProxyMachinePhaseFailed, settings.image, cfgHash, err.Error())
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
			_ = a.upsertMachineStatus(ctx, mg, tiproxyv1alpha1.TiProxyMachinePhaseFailed, settings.image, cfgHash, err.Error())
			return err
		}
		justStarted = true
	}

	if !justStarted && cfgHash != a.lastConfigHash {
		if err := a.pushConfig(ctx, settings.apiBaseURL, cfgBytes); err != nil {
			_ = a.upsertMachineStatus(ctx, mg, tiproxyv1alpha1.TiProxyMachinePhaseFailed, settings.image, cfgHash, err.Error())
			return err
		}
	}

	a.lastConfigHash = cfgHash
	phase := tiproxyv1alpha1.TiProxyMachinePhaseRunning
	message := "tiproxy is running"
	if justStarted {
		phase = tiproxyv1alpha1.TiProxyMachinePhaseStarting
		message = "tiproxy container started with rendered config"
	}
	return a.upsertMachineStatus(ctx, mg, phase, settings.image, cfgHash, message)
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

func (a *Agent) upsertMachineStatus(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, phase, image, cfgHash, message string) error {
	key := types.NamespacedName{
		Namespace: a.Namespace,
		Name:      a.MachineID,
	}
	machine := &tiproxyv1alpha1.TiProxyMachine{}
	if err := a.Client.Get(ctx, key, machine); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		machine = &tiproxyv1alpha1.TiProxyMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      a.MachineID,
				Namespace: a.Namespace,
				Labels: map[string]string{
					"tiproxy.pingcap.com/machine-group": mg.Name,
				},
			},
			Spec: tiproxyv1alpha1.TiProxyMachineSpec{
				MachineGroupRef:    tiproxyv1alpha1.ObjectReference{Name: mg.Name, Namespace: mg.Namespace},
				ProviderInstanceID: a.MachineID,
			},
		}
		if err := controllerutil.SetOwnerReference(mg, machine, a.Scheme); err == nil {
			// Best effort owner reference.
		}
		if err := a.Client.Create(ctx, machine); err != nil {
			return err
		}
	} else {
		base := machine.DeepCopy()
		if machine.Labels == nil {
			machine.Labels = map[string]string{}
		}
		machine.Labels["tiproxy.pingcap.com/machine-group"] = mg.Name
		machine.Spec.MachineGroupRef = tiproxyv1alpha1.ObjectReference{Name: mg.Name, Namespace: mg.Namespace}
		machine.Spec.ProviderInstanceID = a.MachineID
		if !reflect.DeepEqual(base.Labels, machine.Labels) || !reflect.DeepEqual(base.Spec, machine.Spec) {
			if err := a.Client.Patch(ctx, machine, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}

	if err := a.Client.Get(ctx, key, machine); err != nil {
		return err
	}
	statusBase := machine.DeepCopy()
	machine.Status.ObservedGeneration = mg.Generation
	machine.Status.Phase = phase
	machine.Status.ConfigHash = cfgHash
	machine.Status.TiProxyImage = image
	machine.Status.Message = message
	machine.Status.LastHeartbeatTime = metav1.Now()
	conditionStatus := metav1.ConditionTrue
	reason := "Healthy"
	if phase == tiproxyv1alpha1.TiProxyMachinePhaseFailed {
		conditionStatus = metav1.ConditionFalse
		reason = "SyncFailed"
	}
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               tiproxyv1alpha1.ConditionReady,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: mg.Generation,
		LastTransitionTime: metav1.Now(),
	})
	return a.Client.Status().Patch(ctx, machine, client.MergeFrom(statusBase))
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
