// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	nodeMaintenanceFinalizer = "metal.ironcore.dev/cloud-provider-metal"

	labelKeyManagedBy      = "app.kubernetes.io/managed-by"
	cloudProviderMetalName = "cloud-provider-metal"

	serverMaintenancePriority = int32(100)
)

type NodeReconciler struct {
	metalClient  client.Client
	targetClient client.Client
	informer     ctrlcache.Informer
	queue        workqueue.TypedRateLimitingInterface[types.NamespacedName]
}

func NewNodeReconciler(targetClient client.Client, metalClient client.Client, nodeInformer ctrlcache.Informer) NodeReconciler {
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](BaseReconcilerDelay, MaxReconcilerDelay)
	queue := workqueue.NewTypedRateLimitingQueue(rateLimiter)
	return NodeReconciler{
		targetClient: targetClient,
		metalClient:  metalClient,
		informer:     nodeInformer,
		queue:        queue,
	}
}

func (r *NodeReconciler) Start(ctx context.Context) error {
	defer r.queue.ShutDown()

	_, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Errorf("unexpected object type: %T", obj)
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(node))
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if !ok {
				klog.Errorf("unexpected object type: %T", newObj)
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(node))
		},
		DeleteFunc: func(obj any) {
			if deleted, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = deleted.Obj
			}
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Errorf("unexpected object type: %T", obj)
				return
			}
			key := client.ObjectKeyFromObject(node)
			r.queue.Add(key)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}
	go func() {
		for {
			key, quit := r.queue.Get()
			if quit {
				return
			}

			func() {
				defer r.queue.Done(key)

				if err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					klog.Errorf("Failed to reconcile Node %s: %v", key, err)
					r.queue.AddRateLimited(key)
					return
				}

				r.queue.Forget(key)
			}()
		}
	}()
	<-ctx.Done()
	klog.Info("Stopping Node reconciler")
	return nil
}

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) error {
	klog.V(2).Infof("Reconciling Node %s", req.NamespacedName)

	node := &corev1.Node{}
	if err := r.targetClient.Get(ctx, req.NamespacedName, node); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		klog.V(2).Infof("Node %s not found, skipping reconciliation", req.NamespacedName)
		return nil
	}

	if !node.DeletionTimestamp.IsZero() {
		klog.V(2).Infof("Node %s is deleting, reconciling delete flow", req.NamespacedName)
		return r.reconcileDelete(ctx, node)
	}

	if err := r.reconcilePodCIDR(ctx, node); err != nil {
		return fmt.Errorf("unable to reconcile PodCIDR: %w", err)
	}

	if err := r.reconcileMaintenance(ctx, node); err != nil {
		return fmt.Errorf("unable to reconcile maintenance: %w", err)
	}

	return nil
}

func parseProviderID(providerID string) (types.NamespacedName, error) {
	if providerID == "" {
		return types.NamespacedName{}, errors.New("empty providerID")
	}

	provider, rest, ok := strings.Cut(providerID, "://")
	if !ok || provider == "" {
		return types.NamespacedName{}, errors.New("missing scheme")
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return types.NamespacedName{}, errors.New("unexpected count of forward slashes")
	}

	if parts[0] == "" || parts[1] == "" {
		return types.NamespacedName{}, errors.New("missing namespace or name")
	}

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}

func (r *NodeReconciler) reconcileDelete(ctx context.Context, node *corev1.Node) error {
	if !controllerutil.ContainsFinalizer(node, nodeMaintenanceFinalizer) {
		return nil
	}

	serverClaimKey, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		klog.Errorf("Node %s has empty/invalid spec.providerID during node deletion: %v. Skipping CR cleanup to unblock node deletion", node.Name, err)

		base := node.DeepCopy()
		if removed := controllerutil.RemoveFinalizer(node, nodeMaintenanceFinalizer); removed {
			if patchErr := r.targetClient.Patch(ctx, node, client.MergeFrom(base)); patchErr != nil {
				return fmt.Errorf("unable to remove finalizer: %w", patchErr)
			}
		}

		return nil
	}

	if err = r.ensureServerMaintenanceNotExists(ctx, serverClaimKey); err != nil {
		return fmt.Errorf("unable to cleanup ServerMaintenance: %w", err)
	}

	base := node.DeepCopy()
	if removed := controllerutil.RemoveFinalizer(node, nodeMaintenanceFinalizer); removed {
		if err := r.targetClient.Patch(ctx, node, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("unable to remove finalizer: %w", err)
		}
	}

	return nil
}

func (r *NodeReconciler) ensureServerMaintenanceNotExists(ctx context.Context, key types.NamespacedName) error {

	maintenance := &metalv1alpha1.ServerMaintenance{}
	if err := r.metalClient.Get(ctx, key, maintenance); err != nil {
		return client.IgnoreNotFound(err)
	}

	if maintenance.Labels == nil || maintenance.Labels[labelKeyManagedBy] != cloudProviderMetalName {
		klog.Infof("ServerMaintenance %s exists but is not managed by CCM, skipping deletion", key)
		return nil
	}

	if err := r.metalClient.Delete(ctx, maintenance); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (r *NodeReconciler) reconcilePodCIDR(ctx context.Context, node *corev1.Node) error {
	if PodPrefixSize <= 0 {
		// <= 0 disables automatic assignment of pod CIDR.
		return nil
	}

	if node.Spec.PodCIDR != "" {
		klog.InfoS("PodCIDR is already populated; patch was not done", "Node", node.Name, "PodCIDR", node.Spec.PodCIDR)
		return nil
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ip := net.ParseIP(addr.Address)
			if ip == nil {
				return fmt.Errorf("invalid IP address format")
			}

			maskedIP := zeroHostBits(ip, PodPrefixSize)
			podCIDR := fmt.Sprintf("%s/%d", maskedIP, PodPrefixSize)

			nodeBase := node.DeepCopy()
			node.Spec.PodCIDR = podCIDR
			if node.Spec.PodCIDRs == nil {
				node.Spec.PodCIDRs = []string{}
			}
			node.Spec.PodCIDRs = append(node.Spec.PodCIDRs, podCIDR)

			if err := r.targetClient.Patch(ctx, node, client.MergeFrom(nodeBase)); err != nil {
				return fmt.Errorf("failed to patch Node's PodCIDR with error %w", err)
			}

			klog.InfoS("Patched Node's PodCIDR and PodCIDRs", "Node", node.Name, "PodCIDR", podCIDR)

			return nil
		}
	}

	klog.Info("Node does not have a NodeInternalIP, not setting podCIDR")
	return nil
}

func zeroHostBits(ip net.IP, maskSize int) net.IP {
	if ip.To4() != nil {
		mask := net.CIDRMask(maskSize, 32)
		return ip.Mask(mask)
	}

	mask := net.CIDRMask(maskSize, 128)
	return ip.Mask(mask)
}

func (r *NodeReconciler) reconcileMaintenance(ctx context.Context, node *corev1.Node) error {
	serverClaimKey, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return fmt.Errorf("unable to parse provider ID: %w", err)
	}

	serverClaim := &metalv1alpha1.ServerClaim{}
	if err = r.metalClient.Get(ctx, serverClaimKey, serverClaim); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(2).Infof("ServerClaim %s not found, skipping reconciliation", serverClaimKey)
			return nil
		}
		return fmt.Errorf("unable to get ServerClaim: %w", err)
	}

	if serverClaim.Spec.ServerRef == nil {
		klog.V(2).Infof("ServerClaim %s has empty ServerRef, skipping maintenance logic", serverClaimKey)
		return nil
	}

	maintenanceKey := serverClaimKey
	maintenanceRequested := node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] == TrueStr

	if maintenanceRequested {
		base := node.DeepCopy()
		if added := controllerutil.AddFinalizer(node, nodeMaintenanceFinalizer); added {
			if err := r.targetClient.Patch(ctx, node, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("unable to add finalizer: %w", err)
			}
		}

		serverName := serverClaim.Spec.ServerRef.Name

		if err := r.ensureServerMaintenanceExists(ctx, maintenanceKey, serverName); err != nil {
			return fmt.Errorf("unable to ensure ServerMaintenance CR exists: %w", err)
		}

	} else {
		if err := r.ensureServerMaintenanceNotExists(ctx, maintenanceKey); err != nil {
			return fmt.Errorf("unable to ensure ServerMaintenance CR not exists: %w", err)
		}

		base := node.DeepCopy()
		if removed := controllerutil.RemoveFinalizer(node, nodeMaintenanceFinalizer); removed {
			if err := r.targetClient.Patch(ctx, node, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("unable to remove finalizer: %w", err)
			}
		}
	}

	maintenanceNeeded := serverClaim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] == TrueStr
	maintenanceApproved := node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] == TrueStr

	shouldHaveApproval := maintenanceNeeded && maintenanceApproved
	hasApproval := serverClaim.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] == TrueStr

	if shouldHaveApproval != hasApproval {
		if err := r.syncServerClaimApproval(ctx, serverClaim, shouldHaveApproval); err != nil {
			return fmt.Errorf("unable to sync ServerClaim approval: %w", err)
		}
	}

	return nil
}

func (r *NodeReconciler) ensureServerMaintenanceExists(ctx context.Context, key types.NamespacedName, serverName string) error {
	maintenance := &metalv1alpha1.ServerMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	_, err := controllerutil.CreateOrPatch(ctx, r.metalClient, maintenance, func() error {

		maintenance.Spec.Policy = metalv1alpha1.ServerMaintenancePolicyOwnerApproval
		maintenance.Spec.Priority = serverMaintenancePriority
		maintenance.Spec.ServerRef = &corev1.LocalObjectReference{
			Name: serverName,
		}

		if maintenance.Labels == nil {
			maintenance.Labels = make(map[string]string)
		}
		maintenance.Labels[labelKeyManagedBy] = cloudProviderMetalName

		return nil
	})

	return err
}

func (r *NodeReconciler) syncServerClaimApproval(ctx context.Context, serverClaim *metalv1alpha1.ServerClaim, shouldHaveApproval bool) error {
	base := serverClaim.DeepCopy()

	if shouldHaveApproval {
		if serverClaim.Labels == nil {
			serverClaim.Labels = make(map[string]string)
		}
		serverClaim.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr

	} else {
		delete(serverClaim.Labels, metalv1alpha1.ServerMaintenanceApprovalKey)
	}

	if err := r.metalClient.Patch(ctx, serverClaim, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("unable to patch ServerClaim approval label: %w", err)
	}

	return nil
}
