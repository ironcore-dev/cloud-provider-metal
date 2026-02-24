// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
)

type NodeMaintenanceReconciler struct {
	metalClient  client.Client
	targetClient client.Client
	informer     ctrlcache.Informer
	queue        workqueue.TypedRateLimitingInterface[types.NamespacedName]
}

func NewNodeMaintenanceReconciler(targetClient client.Client, metalClient client.Client, nodeInformer ctrlcache.Informer) NodeMaintenanceReconciler {
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](BaseReconcilerDelay, MaxReconcilerDelay)
	queue := workqueue.NewTypedRateLimitingQueue(rateLimiter)
	return NodeMaintenanceReconciler{
		targetClient: targetClient,
		metalClient:  metalClient,
		informer:     nodeInformer,
		queue:        queue,
	}
}

func (r *NodeMaintenanceReconciler) Start(ctx context.Context) error {
	l := ctrl.LoggerFrom(ctx).WithName("node-maintenance-controller")

	_, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				l.Error(nil, "unexpected object type in AddFunc", "type", fmt.Sprintf("%T", obj))
				return
			}

			key := client.ObjectKeyFromObject(node)
			r.queue.Add(key)
		},
		UpdateFunc: func(oldObj, newObj any) {
			node, ok := newObj.(*corev1.Node)
			if !ok {
				l.Error(nil, "unexpected object type in UpdateFunc", "type", fmt.Sprintf("%T", newObj))
				return
			}

			key := client.ObjectKeyFromObject(node)
			r.queue.Add(key)
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
	defer r.queue.ShutDown()
	if err != nil {
		return fmt.Errorf("unable to add event handler: %w", err)
	}

	go func() {
		for {
			key, quit := r.queue.Get()
			if quit {
				return
			}

			func() {
				defer r.queue.Done(key)

				reqL := l.WithValues("namespace", key.Namespace, "node-name", key.Name)
				reqCtx := ctrl.LoggerInto(ctx, reqL)

				if err = r.Reconcile(reqCtx, ctrl.Request{NamespacedName: key}); err != nil {
					reqL.Error(err, "Failed to reconcile Node Maintenance")
					r.queue.AddRateLimited(key)
					return
				}

				r.queue.Forget(key)
			}()
		}
	}()

	<-ctx.Done()

	l.Info("Stopping NodeMaintenance reconciler")
	return nil
}

func (r *NodeMaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) error {
	l := ctrl.LoggerFrom(ctx)

	node := &corev1.Node{}
	if err := r.targetClient.Get(ctx, req.NamespacedName, node); err != nil {
		return client.IgnoreNotFound(err)
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

	serverClaim := &metalv1alpha1.ServerClaim{}
	if err = r.metalClient.Get(ctx, serverClaimKey, serverClaim); err != nil {
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
		serverName := serverClaim.Spec.ServerRef.Name

		if err = r.ensureServerMaintenanceExists(ctx, maintenanceKey, serverName); err != nil {
			return fmt.Errorf("unable to ensure ServerMaintenance CR exists: %w", err)
		}

	} else {
		if err = r.ensureServerMaintenanceNotExists(ctx, maintenanceKey); err != nil {
			return fmt.Errorf("unable to ensure ServerMaintenance CR not exists: %w", err)
		}
	}

	maintenanceNeeded := serverClaim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] == TrueStr
	maintenanceApproved := node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] == TrueStr

	shouldHaveApproval := maintenanceNeeded && maintenanceApproved
	hasApproval := serverClaim.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] == TrueStr

	if shouldHaveApproval != hasApproval {
		if err = r.syncServerClaimApproval(ctx, serverClaim, shouldHaveApproval); err != nil {
			return fmt.Errorf("unable to sync ServerClaim approval: %w", err)
		}
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

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}

func (r *NodeMaintenanceReconciler) ensureServerMaintenanceExists(ctx context.Context, key types.NamespacedName, serverName string) error {
	maintenance := &metalv1alpha1.ServerMaintenance{}

	err := r.metalClient.Get(ctx, key, maintenance)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("unable to get ServerMaintenance: %w", err)
	}

	maintenance = &metalv1alpha1.ServerMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
		Spec: metalv1alpha1.ServerMaintenanceSpec{
			Policy:   metalv1alpha1.ServerMaintenancePolicyOwnerApproval,
			Priority: 100,
			ServerRef: &corev1.LocalObjectReference{
				Name: serverName,
			},
		},
	}

	if err = r.metalClient.Create(ctx, maintenance); err != nil {
		return fmt.Errorf("unable to create ServerMaintenance: %w", err)
	}

	return nil
}

func (r *NodeMaintenanceReconciler) ensureServerMaintenanceNotExists(ctx context.Context, key types.NamespacedName) error {
	maintenance := &metalv1alpha1.ServerMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	if err := r.metalClient.Delete(ctx, maintenance); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("unable to delete ServerMaintenance: %w", err)
	}

	return nil
}

func (r *NodeMaintenanceReconciler) syncServerClaimApproval(ctx context.Context, serverClaim *metalv1alpha1.ServerClaim, shouldHaveApproval bool) error {
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
