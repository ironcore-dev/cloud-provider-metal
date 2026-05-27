// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type ServerClaimReconciler struct {
	metalClient  client.Client
	targetClient client.Client
	informer     ctrlcache.Informer
	queue        workqueue.TypedRateLimitingInterface[types.NamespacedName]
}

func NewServerClaimReconciler(targetClient client.Client, metalClient client.Client, claimInformer ctrlcache.Informer) ServerClaimReconciler {
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](BaseReconcilerDelay, MaxReconcilerDelay)
	queue := workqueue.NewTypedRateLimitingQueue(rateLimiter)
	return ServerClaimReconciler{
		targetClient: targetClient,
		metalClient:  metalClient,
		informer:     claimInformer,
		queue:        queue,
	}
}

func (r *ServerClaimReconciler) Start(ctx context.Context) error {
	_, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			claim, ok := obj.(*metalv1alpha1.ServerClaim)
			if !ok {
				klog.ErrorS(nil, "unexpected object type", "type", fmt.Sprintf("%T", obj))
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(claim))
		},
		UpdateFunc: func(oldObj, newObj any) {
			claim, ok := newObj.(*metalv1alpha1.ServerClaim)
			if !ok {
				klog.ErrorS(nil, "unexpected object type", "type", fmt.Sprintf("%T", newObj))
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(claim))
		},
		DeleteFunc: func(obj any) {
			if deleted, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = deleted.Obj
			}
			claim, ok := obj.(*metalv1alpha1.ServerClaim)
			if !ok {
				klog.ErrorS(nil, "unexpected object type", "type", fmt.Sprintf("%T", obj))
				return
			}
			key := client.ObjectKeyFromObject(claim)
			r.queue.Forget(key)
			r.queue.Done(key)
		},
	})
	defer r.queue.ShutDown()
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}
	go func() {
		for {
			key, quit := r.queue.Get()
			if quit {
				return
			}
			if err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				klog.ErrorS(err, "Failed to reconcile ServerClaim", "serverclaim", key)
				r.queue.AddRateLimited(key)
			}
			r.queue.Done(key)
		}
	}()
	<-ctx.Done()
	klog.InfoS("Stopping ServerClaim reconciler")
	return nil
}

func (r *ServerClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) error {
	klog.V(2).InfoS("Reconciling ServerClaim", "serverclaim", req.NamespacedName)

	serverClaim := &metalv1alpha1.ServerClaim{}
	if err := r.metalClient.Get(ctx, req.NamespacedName, serverClaim); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		klog.V(2).InfoS("ServerClaim not found, skipping reconciliation", "serverclaim", req.NamespacedName)
		return nil
	}

	providerID := fmt.Sprintf("%s://%s/%s", ProviderName, serverClaim.Namespace, serverClaim.Name)
	var nodes corev1.NodeList
	err := r.targetClient.List(ctx, &nodes, client.MatchingFields{NodeProviderIDField: providerID})
	if err != nil {
		return fmt.Errorf("failed to list nodes with providerID %s: %w", providerID, err)
	}
	if len(nodes.Items) == 0 {
		klog.V(2).InfoS("No nodes found", "providerID", providerID)
		return nil
	}
	if len(nodes.Items) > 1 {
		return fmt.Errorf("multiple nodes found with providerID %s", providerID)
	}
	node := nodes.Items[0]
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	originalNode := node.DeepCopy()
	maintenanceVal := serverClaim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey]
	if maintenanceVal == TrueStr {
		node.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] = TrueStr
	} else {
		delete(node.Labels, metalv1alpha1.ServerMaintenanceNeededLabelKey)
	}
	return r.targetClient.Patch(ctx, &node, client.MergeFrom(originalNode))
}
