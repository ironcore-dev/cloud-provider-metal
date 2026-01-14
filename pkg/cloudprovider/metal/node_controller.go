// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
				klog.Errorf("Failed to reconcile Node %s: %v", key, err)
				r.queue.AddRateLimited(key)
			}
			r.queue.Done(key)
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

	claimName, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return err
	}

	claim := &metalv1alpha1.ServerClaim{}
	if err := r.metalClient.Get(ctx, claimName, claim); err != nil {
		return err
	}
	if claim.Labels == nil {
		claim.Labels = make(map[string]string)
	}
	maintenanceVal := claim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey]
	approvalVal := node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey]
	originalClaim := claim.DeepCopy()
	if maintenanceVal == TrueStr && approvalVal == TrueStr {
		claim.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr
	} else {
		delete(claim.Labels, metalv1alpha1.ServerMaintenanceApprovalKey)
	}
	if reflect.DeepEqual(claim, originalClaim) {
		return nil
	}
	return r.metalClient.Patch(ctx, claim, client.MergeFrom(originalClaim))
}

func parseProviderID(providerID string) (types.NamespacedName, error) {
	if providerID == "" {
		return types.NamespacedName{}, errors.New("empty providerID")
	}
	provider, rest, ok := strings.Cut(providerID, "://")
	if !ok || provider == "" {
		return types.NamespacedName{}, errors.New("invalid providerID: missing scheme")
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return types.NamespacedName{}, errors.New("invalid providerID: unexpected count of forward slashes")
	}
	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}
