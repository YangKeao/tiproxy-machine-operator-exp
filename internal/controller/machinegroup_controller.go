package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud/noop"
	"github.com/YangKeao/tiproxy-machine-operator/internal/portallocator"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	machineGroupFinalizer   = "tiproxy.pingcap.com/machinegroup-finalizer"
	maxMachinePortsPerGroup = 50
)

type MachineGroupReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	CloudManager cloud.Manager
	Provider     string
	// MachineStatusStaleAfter defines how long to keep a machine heartbeat
	// without updates before pruning it from status.machines.
	// Set <= 0 to disable pruning.
	MachineStatusStaleAfter time.Duration
}

func (r *MachineGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mg := &tiproxyv1alpha1.TiProxyMachineGroup{}
	if err := r.Get(ctx, req.NamespacedName, mg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mg.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, mg)
	}
	if !controllerutil.ContainsFinalizer(mg, machineGroupFinalizer) {
		base := mg.DeepCopy()
		controllerutil.AddFinalizer(mg, machineGroupFinalizer)
		if err := r.Patch(ctx, mg, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := mg.Spec.PortRange.Validate(); err != nil {
		return r.markMachineGroupError(ctx, mg, "InvalidPortRange", err.Error(), 15*time.Second)
	}
	loadBalancers, err := mg.Spec.Exposure.NormalizeLoadBalancers()
	if err != nil {
		return r.markMachineGroupError(ctx, mg, "InvalidExposure", err.Error(), 15*time.Second)
	}

	links, err := r.listManagedLinks(ctx, mg)
	if err != nil {
		return ctrl.Result{}, err
	}

	machinePorts, err := r.listManagedMachinePorts(ctx, mg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(machinePorts) > maxMachinePortsPerGroup {
		msg := fmt.Sprintf("too many TiProxyMachinePort resources: %d (max %d)", len(machinePorts), maxMachinePortsPerGroup)
		if err := r.syncMachinePortAllocationFailure(ctx, machinePorts, msg); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.syncLinkStatuses(ctx, links, nil, nil, msg); err != nil {
			return ctrl.Result{}, err
		}
		return r.markMachineGroupError(ctx, mg, "TooManyMachinePorts", msg, 20*time.Second)
	}

	portRequests := make([]portallocator.LinkPortRequest, 0, len(machinePorts))
	for i := range machinePorts {
		portRequests = append(portRequests, portallocator.LinkPortRequest{
			UID:           machinePorts[i].UID,
			Name:          machinePorts[i].Namespace + "/" + machinePorts[i].Name,
			RequestedPort: machinePorts[i].Spec.RequestedPort,
		})
	}

	allocatedPorts, err := portallocator.ReconcilePorts(mg.Status.AllocatedPorts, portRequests, mg.Spec.PortRange.Start, mg.Spec.PortRange.End)
	if err != nil {
		msg := err.Error()
		if err := r.syncMachinePortAllocationFailure(ctx, machinePorts, msg); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.syncLinkStatuses(ctx, links, nil, nil, "port allocation failed: "+msg); err != nil {
			return ctrl.Result{}, err
		}
		return r.markMachineGroupError(ctx, mg, "PortAllocationFailed", msg, 10*time.Second)
	}

	linkPorts, linkErrors := assignPortsToLinks(links, machinePorts, allocatedPorts)
	resolvedLinks := buildResolvedLinks(links, linkPorts)

	manager, err := r.cloudManagerFor(mg)
	if err != nil {
		return ctrl.Result{}, err
	}
	cloudStatus, machinePortCloud, cloudErr := manager.EnsureMachineGroup(ctx, mg, machinePorts, allocatedPorts)

	prunedMachines := r.pruneStaleMachineStatuses(mg, time.Now())
	if prunedMachines > 0 {
		ctrl.LoggerFrom(ctx).Info("pruned stale machine statuses", "count", prunedMachines, "staleAfter", r.MachineStatusStaleAfter.String())
	}

	statusBase := mg.DeepCopy()
	mg.Status.ObservedGeneration = mg.Generation
	mg.Status.AllocatedPorts = allocatedPorts
	mg.Status.ResolvedLinks = resolvedLinks
	mg.Status.Cloud = cloudStatus

	switch {
	case cloudErr != nil:
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "CloudEnsureFailed",
			Message:            cloudErr.Error(),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
	case len(linkErrors) > 0:
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InsufficientMachinePorts",
			Message:            fmt.Sprintf("%d link(s) unresolved due to missing machine ports", len(linkErrors)),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
	default:
		readyMsg := fmt.Sprintf("resolved %d link(s), allocated %d machine port(s)", len(resolvedLinks), len(allocatedPorts))
		if prunedMachines == 1 {
			readyMsg += ", pruned 1 stale machine status entry"
		} else if prunedMachines > 1 {
			readyMsg += fmt.Sprintf(", pruned %d stale machine status entries", prunedMachines)
		}
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            readyMsg,
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if err := r.Status().Patch(ctx, mg, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.syncMachinePortStatuses(ctx, mg, loadBalancers, machinePorts, allocatedPorts, machinePortCloud, cloudErr); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncLinkStatuses(ctx, links, linkPorts, linkErrors, "no allocated TiProxyMachinePort is available for this link"); err != nil {
		return ctrl.Result{}, err
	}

	if cloudErr != nil || len(linkErrors) > 0 {
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *MachineGroupReconciler) reconcileDelete(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mg, machineGroupFinalizer) {
		return ctrl.Result{}, nil
	}
	manager, err := r.cloudManagerFor(mg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := manager.DeleteMachineGroup(ctx, mg); err != nil {
		return ctrl.Result{RequeueAfter: 20 * time.Second}, err
	}
	base := mg.DeepCopy()
	controllerutil.RemoveFinalizer(mg, machineGroupFinalizer)
	if err := r.Patch(ctx, mg, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *MachineGroupReconciler) listManagedLinks(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) ([]tiproxyv1alpha1.TiDBResourcePoolLink, error) {
	var list tiproxyv1alpha1.TiDBResourcePoolLinkList
	opts := []client.ListOption{client.InNamespace(mg.Namespace)}
	if mg.Spec.LinkSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(mg.Spec.LinkSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid spec.linkSelector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil, err
	}

	result := make([]tiproxyv1alpha1.TiDBResourcePoolLink, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Spec.Disabled {
			continue
		}
		ref := item.Spec.MachineGroupRef.NamespacedName(item.Namespace)
		if ref.Name != mg.Name || ref.Namespace != mg.Namespace {
			continue
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b tiproxyv1alpha1.TiDBResourcePoolLink) int {
		keyA := a.Namespace + "/" + a.Name
		keyB := b.Namespace + "/" + b.Name
		if keyA < keyB {
			return -1
		}
		if keyA > keyB {
			return 1
		}
		return 0
	})
	return result, nil
}

func (r *MachineGroupReconciler) listManagedMachinePorts(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) ([]tiproxyv1alpha1.TiProxyMachinePort, error) {
	var list tiproxyv1alpha1.TiProxyMachinePortList
	if err := r.List(ctx, &list, client.InNamespace(mg.Namespace)); err != nil {
		return nil, err
	}

	result := make([]tiproxyv1alpha1.TiProxyMachinePort, 0, len(list.Items))
	for _, item := range list.Items {
		ref := item.Spec.MachineGroupRef.NamespacedName(item.Namespace)
		if ref.Name != mg.Name || ref.Namespace != mg.Namespace {
			continue
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b tiproxyv1alpha1.TiProxyMachinePort) int {
		keyA := a.Namespace + "/" + a.Name
		keyB := b.Namespace + "/" + b.Name
		if keyA < keyB {
			return -1
		}
		if keyA > keyB {
			return 1
		}
		return 0
	})
	return result, nil
}

func assignPortsToLinks(
	links []tiproxyv1alpha1.TiDBResourcePoolLink,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	allocated map[string]int32,
) (map[string]int32, map[string]string) {
	assigned := make(map[string]int32, len(links))
	errors := make(map[string]string)

	type machinePortAllocation struct {
		Port int32
		Key  string
	}
	orderedAvailable := make([]machinePortAllocation, 0, len(machinePorts))
	for i := range machinePorts {
		port, ok := allocated[string(machinePorts[i].UID)]
		if !ok || port <= 0 {
			continue
		}
		a := machinePortAllocation{
			Port: port,
			Key:  machinePorts[i].Namespace + "/" + machinePorts[i].Name,
		}
		orderedAvailable = append(orderedAvailable, a)
	}

	if len(links) == 0 {
		return assigned, errors
	}
	if len(orderedAvailable) == 0 {
		for i := range links {
			errors[string(links[i].UID)] = "no TiProxyMachinePort has an allocated port"
		}
		return assigned, errors
	}

	used := make(map[int32]struct{}, len(links))

	portByName := make(map[string]int32, len(orderedAvailable))
	for _, a := range orderedAvailable {
		if _, taken := used[a.Port]; taken {
			continue
		}
		if _, exists := portByName[a.Key]; !exists {
			portByName[a.Key] = a.Port
		}
	}
	for i := range links {
		uid := string(links[i].UID)
		if _, ok := assigned[uid]; ok {
			continue
		}
		nameKey := links[i].Namespace + "/" + links[i].Name
		port, ok := portByName[nameKey]
		if !ok {
			continue
		}
		assigned[uid] = port
		used[port] = struct{}{}
		delete(portByName, nameKey)
	}

	remaining := make([]int32, 0, len(orderedAvailable))
	for _, a := range orderedAvailable {
		if _, taken := used[a.Port]; taken {
			continue
		}
		remaining = append(remaining, a.Port)
	}
	slices.Sort(remaining)
	next := 0
	for i := range links {
		uid := string(links[i].UID)
		if _, ok := assigned[uid]; ok {
			continue
		}
		if next >= len(remaining) {
			break
		}
		assigned[uid] = remaining[next]
		next++
	}

	if len(assigned) < len(links) {
		msg := fmt.Sprintf("insufficient TiProxyMachinePort resources: %d link(s), %d allocated port(s)", len(links), len(orderedAvailable))
		for i := range links {
			uid := string(links[i].UID)
			if _, ok := assigned[uid]; ok {
				continue
			}
			errors[uid] = msg
		}
	}
	return assigned, errors
}

func buildResolvedLinks(links []tiproxyv1alpha1.TiDBResourcePoolLink, linkPorts map[string]int32) []tiproxyv1alpha1.ResolvedLinkStatus {
	resolved := make([]tiproxyv1alpha1.ResolvedLinkStatus, 0, len(linkPorts))
	for i := range links {
		port, ok := linkPorts[string(links[i].UID)]
		if !ok {
			continue
		}
		clusterName := strings.TrimSpace(links[i].Spec.ClusterName)
		if clusterName == "" {
			clusterName = fmt.Sprintf("%s.%s", links[i].Namespace, links[i].Name)
		}
		routeNamespace := strings.TrimSpace(links[i].Spec.RouteNamespace)
		if routeNamespace == "" {
			routeNamespace = links[i].Name
		}
		entry := tiproxyv1alpha1.ResolvedLinkStatus{
			UID:            string(links[i].UID),
			Name:           links[i].Name,
			Namespace:      links[i].Namespace,
			RouteNamespace: routeNamespace,
			FrontendUser:   links[i].Spec.FrontendUser,
			Port:           port,
			ClusterName:    clusterName,
			PDAddresses:    slices.Clone(links[i].Spec.PDAddresses),
			NSServerAddr:   links[i].Spec.NSServerAddr,
			TLSSecretRef:   links[i].Spec.TLSSecretRef,
		}
		entry.ConfigHash = resolvedLinkHash(entry)
		resolved = append(resolved, entry)
	}
	return resolved
}

func (r *MachineGroupReconciler) syncMachinePortStatuses(
	ctx context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	loadBalancers []tiproxyv1alpha1.LoadBalancerExposureSpec,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	allocated map[string]int32,
	cloud map[string]tiproxyv1alpha1.MachinePortCloudStatus,
	cloudErr error,
) error {
	exposureDisabled := len(loadBalancers) == 0
	for i := range machinePorts {
		port := machinePorts[i].DeepCopy()
		base := port.DeepCopy()
		uid := string(port.UID)

		assigned, hasAssigned := allocated[uid]
		if hasAssigned {
			port.Status.AssignedPort = new(int32)
			*port.Status.AssignedPort = assigned
			port.Status.Phase = tiproxyv1alpha1.MachinePortPhaseAllocated
			apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
				Type:               tiproxyv1alpha1.ConditionPortAssigned,
				Status:             metav1.ConditionTrue,
				Reason:             "Assigned",
				Message:            fmt.Sprintf("assigned port %d", assigned),
				ObservedGeneration: port.Generation,
				LastTransitionTime: metav1.Now(),
			})
		} else {
			port.Status.AssignedPort = nil
			port.Status.Phase = tiproxyv1alpha1.MachinePortPhasePending
			apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
				Type:               tiproxyv1alpha1.ConditionPortAssigned,
				Status:             metav1.ConditionFalse,
				Reason:             "Unassigned",
				Message:            "waiting for port allocation",
				ObservedGeneration: port.Generation,
				LastTransitionTime: metav1.Now(),
			})
		}

		port.Status.Cloud = tiproxyv1alpha1.MachinePortCloudStatus{}
		if hasAssigned {
			switch {
			case exposureDisabled:
				port.Status.Phase = tiproxyv1alpha1.MachinePortPhaseReady
				apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
					Type:               tiproxyv1alpha1.ConditionExposed,
					Status:             metav1.ConditionTrue,
					Reason:             "ExposureDisabled",
					Message:            "machine group load balancer exposure is disabled",
					ObservedGeneration: port.Generation,
					LastTransitionTime: metav1.Now(),
				})
			case cloudErr != nil:
				apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
					Type:               tiproxyv1alpha1.ConditionExposed,
					Status:             metav1.ConditionFalse,
					Reason:             "CloudEnsureFailed",
					Message:            cloudErr.Error(),
					ObservedGeneration: port.Generation,
					LastTransitionTime: metav1.Now(),
				})
			default:
				port.Status.Cloud = cloud[uid]
				port.Status.Cloud.SetPrimaryLoadBalancerCompat()
				readyLoadBalancers := countReadyPortLoadBalancers(loadBalancers, port.Status.Cloud.LoadBalancers)
				requiredLoadBalancers := countRequiredPortLoadBalancers(loadBalancers)
				if requiredLoadBalancers > 0 && readyLoadBalancers == requiredLoadBalancers {
					port.Status.Phase = tiproxyv1alpha1.MachinePortPhaseReady
					apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
						Type:               tiproxyv1alpha1.ConditionExposed,
						Status:             metav1.ConditionTrue,
						Reason:             "Ready",
						Message:            fmt.Sprintf("%d/%d load balancer listeners are ready", readyLoadBalancers, requiredLoadBalancers),
						ObservedGeneration: port.Generation,
						LastTransitionTime: metav1.Now(),
					})
				} else {
					apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
						Type:               tiproxyv1alpha1.ConditionExposed,
						Status:             metav1.ConditionFalse,
						Reason:             "Pending",
						Message:            fmt.Sprintf("waiting for load balancer listeners (%d/%d ready)", readyLoadBalancers, requiredLoadBalancers),
						ObservedGeneration: port.Generation,
						LastTransitionTime: metav1.Now(),
					})
				}
			}
		} else {
			apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
				Type:               tiproxyv1alpha1.ConditionExposed,
				Status:             metav1.ConditionFalse,
				Reason:             "PortUnassigned",
				Message:            "listener is not reconciled before port assignment",
				ObservedGeneration: port.Generation,
				LastTransitionTime: metav1.Now(),
			})
		}

		port.Status.ObservedGeneration = port.Generation
		if err := r.Status().Patch(ctx, port, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

func countRequiredPortLoadBalancers(loadBalancers []tiproxyv1alpha1.LoadBalancerExposureSpec) int {
	return len(loadBalancers)
}

func countReadyPortLoadBalancers(loadBalancers []tiproxyv1alpha1.LoadBalancerExposureSpec, statuses []tiproxyv1alpha1.MachinePortLoadBalancerCloudStatus) int {
	readyByName := make(map[string]struct{}, len(statuses))
	for i := range statuses {
		if strings.TrimSpace(statuses[i].ListenerID) == "" {
			continue
		}
		readyByName[statuses[i].Name] = struct{}{}
	}
	count := 0
	for i := range loadBalancers {
		if _, ok := readyByName[loadBalancers[i].Name]; ok {
			count++
		}
	}
	return count
}

func (r *MachineGroupReconciler) syncMachinePortAllocationFailure(
	ctx context.Context,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	msg string,
) error {
	for i := range machinePorts {
		port := machinePorts[i].DeepCopy()
		base := port.DeepCopy()
		port.Status.ObservedGeneration = port.Generation
		port.Status.AssignedPort = nil
		port.Status.Phase = tiproxyv1alpha1.MachinePortPhaseError
		port.Status.Cloud = tiproxyv1alpha1.MachinePortCloudStatus{}
		apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionPortAssigned,
			Status:             metav1.ConditionFalse,
			Reason:             "PortAllocationFailed",
			Message:            msg,
			ObservedGeneration: port.Generation,
			LastTransitionTime: metav1.Now(),
		})
		apimeta.SetStatusCondition(&port.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionExposed,
			Status:             metav1.ConditionFalse,
			Reason:             "PortAllocationFailed",
			Message:            msg,
			ObservedGeneration: port.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, port, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

func (r *MachineGroupReconciler) syncLinkStatuses(
	ctx context.Context,
	links []tiproxyv1alpha1.TiDBResourcePoolLink,
	assigned map[string]int32,
	errByUID map[string]string,
	defaultErr string,
) error {
	for i := range links {
		link := links[i].DeepCopy()
		base := link.DeepCopy()
		link.Status.ObservedGeneration = link.Generation

		if _, ok := assigned[string(link.UID)]; ok {
			link.Status.Phase = "Ready"
			apimeta.SetStatusCondition(&link.Status.Conditions, metav1.Condition{
				Type:               tiproxyv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Resolved",
				Message:            "resolved to an allocated TiProxyMachinePort",
				ObservedGeneration: link.Generation,
				LastTransitionTime: metav1.Now(),
			})
		} else {
			link.Status.Phase = "Error"
			msg := strings.TrimSpace(errByUID[string(link.UID)])
			if msg == "" {
				msg = strings.TrimSpace(defaultErr)
			}
			apimeta.SetStatusCondition(&link.Status.Conditions, metav1.Condition{
				Type:               tiproxyv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "Unresolved",
				Message:            msg,
				ObservedGeneration: link.Generation,
				LastTransitionTime: metav1.Now(),
			})
		}

		if err := r.Status().Patch(ctx, link, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

func (r *MachineGroupReconciler) markMachineGroupError(
	ctx context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	reason, msg string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	statusBase := mg.DeepCopy()
	apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
		Type:               tiproxyv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: mg.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Patch(ctx, mg, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *MachineGroupReconciler) pruneStaleMachineStatuses(mg *tiproxyv1alpha1.TiProxyMachineGroup, now time.Time) int {
	if mg == nil || len(mg.Status.Machines) == 0 {
		return 0
	}
	if r.MachineStatusStaleAfter <= 0 {
		return 0
	}

	removed := 0
	for machineID, status := range mg.Status.Machines {
		if status.LastHeartbeatTime.IsZero() || now.Sub(status.LastHeartbeatTime.Time) > r.MachineStatusStaleAfter {
			delete(mg.Status.Machines, machineID)
			removed++
		}
	}
	if len(mg.Status.Machines) == 0 {
		mg.Status.Machines = nil
	}
	return removed
}

func resolvedLinkHash(link tiproxyv1alpha1.ResolvedLinkStatus) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(link.UID))
	_, _ = hasher.Write([]byte(link.Name))
	_, _ = hasher.Write([]byte(link.Namespace))
	_, _ = hasher.Write([]byte(link.RouteNamespace))
	_, _ = hasher.Write([]byte(link.FrontendUser))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", link.Port)))
	_, _ = hasher.Write([]byte(link.ClusterName))
	_, _ = hasher.Write([]byte(link.NSServerAddr))
	for _, addr := range link.PDAddresses {
		_, _ = hasher.Write([]byte(addr))
	}
	if link.TLSSecretRef != nil {
		_, _ = hasher.Write([]byte(link.TLSSecretRef.Name))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (r *MachineGroupReconciler) cloudManagerFor(mg *tiproxyv1alpha1.TiProxyMachineGroup) (cloud.Manager, error) {
	provider := strings.ToLower(strings.TrimSpace(mg.Spec.Provider))
	if provider == "" || provider == "none" || provider == "noop" {
		return noop.NewManager(), nil
	}

	if provider != "aws" {
		return nil, fmt.Errorf("unsupported spec.provider %q", provider)
	}

	controllerProvider := strings.ToLower(strings.TrimSpace(r.Provider))
	if controllerProvider == "" {
		controllerProvider = "noop"
	}
	if controllerProvider != "aws" {
		return nil, fmt.Errorf("spec.provider=%q but controller started with --cloud-provider=%q", provider, r.Provider)
	}

	if r.CloudManager == nil {
		return nil, fmt.Errorf("aws cloud manager is not initialized")
	}
	return r.CloudManager, nil
}

func (r *MachineGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToMachineGroup := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		switch v := obj.(type) {
		case *tiproxyv1alpha1.TiDBResourcePoolLink:
			ref := v.Spec.MachineGroupRef.NamespacedName(v.Namespace)
			if ref.Name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: ref}}
		case *tiproxyv1alpha1.TiProxyMachinePort:
			ref := v.Spec.MachineGroupRef.NamespacedName(v.Namespace)
			if ref.Name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: ref}}
		default:
			return nil
		}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&tiproxyv1alpha1.TiProxyMachineGroup{}).
		Watches(&tiproxyv1alpha1.TiDBResourcePoolLink{}, mapToMachineGroup).
		Watches(&tiproxyv1alpha1.TiProxyMachinePort{}, mapToMachineGroup).
		Complete(r)
}
