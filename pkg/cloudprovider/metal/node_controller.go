// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
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

	l := ctrl.LoggerFrom(ctx).WithName("node-controller")

	_, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				l.Error(nil, "unexpected object type in AddFunc", "type", fmt.Sprintf("%T", obj))
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(node))
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if !ok {
				l.Error(nil, "unexpected object type in UpdateFunc", "type", fmt.Sprintf("%T", newObj))
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
				l.Error(nil, "unexpected object type in DeleteFunc", "type", fmt.Sprintf("%T", obj))
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

				reqL := l.WithValues("namespace", key.Namespace, "node-name", key.Name, "reconcile-id", uuid.NewString())
				reqCtx := ctrl.LoggerInto(ctx, reqL)

				if err = r.Reconcile(reqCtx, ctrl.Request{NamespacedName: key}); err != nil {
					reqL.Error(err, "Failed to reconcile Node")
					r.queue.AddRateLimited(key)
					return
				}

				r.queue.Forget(key)
			}()
		}
	}()
	<-ctx.Done()
	l.Info("Stopping Node reconciler")
	return nil
}

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) error {
	l := ctrl.LoggerFrom(ctx)
	l.V(2).Info("Reconciling Node")

	node := &corev1.Node{}
	if err := r.targetClient.Get(ctx, req.NamespacedName, node); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		l.V(2).Info("Node not found, skipping reconciliation")
		return nil
	}

	if node.Spec.ProviderID == "" {
		l.Info("Node has empty spec.providerID, skipping reconciliation")
		return nil
	}

	serverClaimKey, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return fmt.Errorf("unable to parse provider ID: %w", err)
	}

	l = l.WithValues("server-claim-name", serverClaimKey.Name, "server-claim-namespace", serverClaimKey.Namespace)

	if !node.DeletionTimestamp.IsZero() {
		l.Info("Node is deleting, reconciling delete flow")
		return r.reconcileDelete(ctx, node, serverClaimKey)
	}

	if err = r.reconcilePodCIDR(ctx, node); err != nil {
		return fmt.Errorf("unable to reconcile PodCIDR: %w", err)
	}

	if err = r.reconcileMaintenance(ctx, node, serverClaimKey); err != nil {
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

func (r *NodeReconciler) reconcileDelete(ctx context.Context, node *corev1.Node, serverClaimKey types.NamespacedName) error {
	if !controllerutil.ContainsFinalizer(node, nodeMaintenanceFinalizer) {
		return nil
	}

	if err := r.ensureServerMaintenanceNotExists(ctx, serverClaimKey); err != nil {
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
	maintenance := &metalv1alpha1.ServerMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	if err := r.metalClient.Delete(ctx, maintenance); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (r *NodeReconciler) reconcilePodCIDR(ctx context.Context, node *corev1.Node) error {
	l := ctrl.LoggerFrom(ctx)

	if PodPrefixSize <= 0 {
		// <= 0 disables automatic assignment of pod CIDR.
		return nil
	}

	if node.Spec.PodCIDR != "" {
		l.Info("PodCIDR is already populated; patch was not done", "PodCIDR", node.Spec.PodCIDR)
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

			l.Info("Patched Node's PodCIDR and PodCIDRs", "PodCIDR", podCIDR)

			return nil
		}
	}

	l.Info("Node does not have a NodeInternalIP, not setting podCIDR")
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

func (r *NodeReconciler) reconcileMaintenance(ctx context.Context, node *corev1.Node, serverClaimKey types.NamespacedName) error {
	l := ctrl.LoggerFrom(ctx)

	serverClaim := &metalv1alpha1.ServerClaim{}
	if err := r.metalClient.Get(ctx, serverClaimKey, serverClaim); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("ServerClaim not found, skipping reconciliation")
			return nil
		}
		return fmt.Errorf("unable to get ServerClaim: %w", err)
	}

	if serverClaim.Spec.ServerRef == nil {
		l.Info("ServerClaim has empty ServerRef, skipping maintenance logic")
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
