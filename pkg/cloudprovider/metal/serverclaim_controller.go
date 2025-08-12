// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"fmt"
	"time"

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

const (
	nodeProviderIDField string        = ".spec.providerID"
	baseDelay           time.Duration = 5 * time.Second
	maxDelay            time.Duration = 5 * time.Minute
)

type ServerClaimReconciler struct {
	metalClient  client.Client
	targetClient client.Client
	informer     ctrlcache.Informer
	queue        workqueue.TypedRateLimitingInterface[types.NamespacedName]
}

func NewServerClaimReconciler(targetClient client.Client, metalClient client.Client, claimInformer ctrlcache.Informer) ServerClaimReconciler {
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](baseDelay, maxDelay)
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
				klog.Errorf("unexpected object type: %T", obj)
				return
			}
			r.queue.Add(client.ObjectKeyFromObject(claim))
		},
		UpdateFunc: func(oldObj, newObj any) {
			claim, ok := newObj.(*metalv1alpha1.ServerClaim)
			if !ok {
				klog.Errorf("unexpected object type: %T", newObj)
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
				klog.Errorf("unexpected object type: %T", obj)
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
				klog.Errorf("Failed to reconcile ServerClaim %s: %v", key, err)
				r.queue.AddRateLimited(key)
			}
			r.queue.Done(key)
		}
	}()
	<-ctx.Done()
	klog.Info("Stopping ServerClaim reconciler")
	return nil
}

func (r *ServerClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) error {
	klog.V(2).Infof("Reconciling ServerClaim %s", req.NamespacedName)

	serverClaim := &metalv1alpha1.ServerClaim{}
	if err := r.metalClient.Get(ctx, req.NamespacedName, serverClaim); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		klog.V(2).Infof("ServerClaim %s not found, skipping reconciliation", req.NamespacedName)
		return nil
	}

	providerID := fmt.Sprintf("%s://%s/%s", ProviderName, serverClaim.Namespace, serverClaim.Name)
	var nodes corev1.NodeList
	err := r.targetClient.List(ctx, &nodes, client.MatchingFields{nodeProviderIDField: providerID})
	if err != nil {
		return fmt.Errorf("failed to list nodes with providerID %s: %w", providerID, err)
	}
	if len(nodes.Items) == 0 {
		klog.V(2).Infof("No nodes found with providerID %s", providerID)
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
	if maintenanceVal == "true" {
		node.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] = "true"
	} else {
		delete(node.Labels, metalv1alpha1.ServerMaintenanceNeededLabelKey)
	}
	return r.targetClient.Patch(ctx, &node, client.MergeFrom(originalNode))
}
