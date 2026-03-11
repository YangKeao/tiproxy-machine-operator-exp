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
	machineGroupFinalizer = "tiproxy.pingcap.com/machinegroup-finalizer"
)

type MachineGroupReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	CloudManager cloud.Manager
	Provider     string
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
		statusBase := mg.DeepCopy()
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InvalidPortRange",
			Message:            err.Error(),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
		_ = r.Status().Patch(ctx, mg, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	links, err := r.listManagedLinks(ctx, mg)
	if err != nil {
		return ctrl.Result{}, err
	}

	portRequests := make([]portallocator.LinkPortRequest, 0, len(links))
	for _, link := range links {
		portRequests = append(portRequests, portallocator.LinkPortRequest{
			UID:           link.UID,
			Name:          link.Namespace + "/" + link.Name,
			RequestedPort: link.Spec.Port,
		})
	}

	allocatedPorts, err := portallocator.ReconcilePorts(mg.Status.AllocatedPorts, portRequests, mg.Spec.PortRange.Start, mg.Spec.PortRange.End)
	if err != nil {
		statusBase := mg.DeepCopy()
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PortAllocationFailed",
			Message:            err.Error(),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
		_ = r.Status().Patch(ctx, mg, client.MergeFrom(statusBase))
		if errSync := r.syncLinkAllocationFailure(ctx, links, err.Error()); errSync != nil {
			return ctrl.Result{}, errSync
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	resolvedLinks := make([]tiproxyv1alpha1.ResolvedLinkStatus, 0, len(links))
	for _, link := range links {
		uid := string(link.UID)
		clusterName := strings.TrimSpace(link.Spec.ClusterName)
		if clusterName == "" {
			clusterName = fmt.Sprintf("%s.%s", link.Namespace, link.Name)
		}
		resolved := tiproxyv1alpha1.ResolvedLinkStatus{
			UID:            uid,
			Name:           link.Name,
			Namespace:      link.Namespace,
			RouteNamespace: link.Spec.RouteNamespace,
			FrontendUser:   link.Spec.FrontendUser,
			Port:           allocatedPorts[uid],
			ClusterName:    clusterName,
			PDAddresses:    slices.Clone(link.Spec.PDAddresses),
			NSServerAddr:   link.Spec.NSServerAddr,
			TLSSecretRef:   link.Spec.TLSSecretRef,
		}
		if resolved.RouteNamespace == "" {
			resolved.RouteNamespace = link.Name
		}
		resolved.ConfigHash = resolvedLinkHash(resolved)
		resolvedLinks = append(resolvedLinks, resolved)
	}

	manager, err := r.cloudManagerFor(mg)
	if err != nil {
		return ctrl.Result{}, err
	}
	cloudStatus, cloudErr := manager.EnsureMachineGroup(ctx, mg)

	statusBase := mg.DeepCopy()
	mg.Status.ObservedGeneration = mg.Generation
	mg.Status.AllocatedPorts = allocatedPorts
	mg.Status.ResolvedLinks = resolvedLinks
	mg.Status.Cloud = cloudStatus
	if cloudErr != nil {
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "CloudEnsureFailed",
			Message:            cloudErr.Error(),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		apimeta.SetStatusCondition(&mg.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            fmt.Sprintf("resolved %d link(s)", len(resolvedLinks)),
			ObservedGeneration: mg.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
	if err := r.Status().Patch(ctx, mg, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.syncLinkStatuses(ctx, links, allocatedPorts); err != nil {
		return ctrl.Result{}, err
	}

	if cloudErr != nil {
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

func (r *MachineGroupReconciler) listManagedLinks(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) ([]tiproxyv1alpha1.TiDBInstanceLink, error) {
	var list tiproxyv1alpha1.TiDBInstanceLinkList
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

	result := make([]tiproxyv1alpha1.TiDBInstanceLink, 0, len(list.Items))
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
	slices.SortFunc(result, func(a, b tiproxyv1alpha1.TiDBInstanceLink) int {
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

func (r *MachineGroupReconciler) syncLinkStatuses(ctx context.Context, links []tiproxyv1alpha1.TiDBInstanceLink, allocated map[string]int32) error {
	for i := range links {
		link := links[i].DeepCopy()
		port, ok := allocated[string(link.UID)]
		if !ok {
			continue
		}
		needUpdate := link.Status.AssignedPort == nil || *link.Status.AssignedPort != port ||
			link.Status.ObservedGeneration != link.Generation || link.Status.Phase != "Assigned" ||
			!apimeta.IsStatusConditionTrue(link.Status.Conditions, tiproxyv1alpha1.ConditionPortAssigned)
		if !needUpdate {
			continue
		}

		base := link.DeepCopy()
		link.Status.ObservedGeneration = link.Generation
		link.Status.Phase = "Assigned"
		link.Status.AssignedPort = new(int32)
		*link.Status.AssignedPort = port
		apimeta.SetStatusCondition(&link.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionPortAssigned,
			Status:             metav1.ConditionTrue,
			Reason:             "Assigned",
			Message:            fmt.Sprintf("assigned port %d", port),
			ObservedGeneration: link.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, link, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

func (r *MachineGroupReconciler) syncLinkAllocationFailure(ctx context.Context, links []tiproxyv1alpha1.TiDBInstanceLink, msg string) error {
	for i := range links {
		link := links[i].DeepCopy()
		cond := apimeta.FindStatusCondition(link.Status.Conditions, tiproxyv1alpha1.ConditionPortAssigned)
		needUpdate := link.Status.AssignedPort != nil ||
			link.Status.ObservedGeneration != link.Generation || link.Status.Phase != "Error" ||
			cond == nil || cond.Status != metav1.ConditionFalse ||
			cond.Reason != "PortAllocationFailed" || cond.Message != msg
		if !needUpdate {
			continue
		}

		base := link.DeepCopy()
		link.Status.ObservedGeneration = link.Generation
		link.Status.Phase = "Error"
		link.Status.AssignedPort = nil
		apimeta.SetStatusCondition(&link.Status.Conditions, metav1.Condition{
			Type:               tiproxyv1alpha1.ConditionPortAssigned,
			Status:             metav1.ConditionFalse,
			Reason:             "PortAllocationFailed",
			Message:            msg,
			ObservedGeneration: link.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Patch(ctx, link, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
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
		link, ok := obj.(*tiproxyv1alpha1.TiDBInstanceLink)
		if !ok {
			return nil
		}
		ref := link.Spec.MachineGroupRef.NamespacedName(link.Namespace)
		if ref.Name == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: ref},
		}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&tiproxyv1alpha1.TiProxyMachineGroup{}).
		Watches(&tiproxyv1alpha1.TiDBInstanceLink{}, mapToMachineGroup).
		Complete(r)
}
