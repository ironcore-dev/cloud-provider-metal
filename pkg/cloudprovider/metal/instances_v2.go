// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type metalInstancesV2 struct {
	targetClient   client.Client
	metalClient    client.Client
	metalNamespace string
	clusterName    string
}

func newMetalInstancesV2(targetClient client.Client, metalClient client.Client, namespace, clusterName string) cloudprovider.InstancesV2 {
	return &metalInstancesV2{
		targetClient:   targetClient,
		metalClient:    metalClient,
		metalNamespace: namespace,
		clusterName:    clusterName,
	}
}

func (o *metalInstancesV2) InstanceExists(ctx context.Context, node *corev1.Node) (bool, error) {
	if node == nil {
		return false, nil
	}
	klog.V(4).InfoS("Checking if node exists", "Node", node.Name)

	serverClaim, err := o.getServerClaimForNode(ctx, node)
	if err != nil {
		return false, err
	}
	if serverClaim == nil {
		return false, cloudprovider.InstanceNotFound
	}

	klog.V(4).InfoS("Instance for node exists", "Node", node.Name, "ServerClaim", client.ObjectKeyFromObject(serverClaim))
	return true, nil
}

func (o *metalInstancesV2) InstanceShutdown(ctx context.Context, node *corev1.Node) (bool, error) {
	if node == nil {
		return false, nil
	}
	klog.V(4).InfoS("Checking if instance is shut down", "Node", node.Name)

	serverClaim, err := o.getServerClaimForNode(ctx, node)
	if err != nil {
		return false, err
	}
	if serverClaim == nil {
		return false, cloudprovider.InstanceNotFound
	}

	server := &metalv1alpha1.Server{}
	if err := o.metalClient.Get(ctx, client.ObjectKey{Name: serverClaim.Spec.ServerRef.Name}, server); err != nil {
		if apierrors.IsNotFound(err) {
			return false, cloudprovider.InstanceNotFound
		}
		return false, fmt.Errorf("failed to get server object for node %s: %w", node.Name, err)
	}

	nodeShutDownStatus := server.Status.PowerState == metalv1alpha1.ServerOffPowerState
	klog.V(4).InfoS("Instance shut down status", "NodeShutdown", nodeShutDownStatus)
	return nodeShutDownStatus, nil
}

func (o *metalInstancesV2) InstanceMetadata(ctx context.Context, node *corev1.Node) (*cloudprovider.InstanceMetadata, error) {
	if node == nil {
		return nil, nil
	}

	var serverClaim *metalv1alpha1.ServerClaim

	serverClaim, err := o.getServerClaimForNode(ctx, node)
	if err != nil {
		return nil, err
	}

	if serverClaim == nil {
		return nil, cloudprovider.InstanceNotFound
	}

	// Add label for clusterName to server claim object
	serverClaimBase := serverClaim.DeepCopy()
	if serverClaim.Labels == nil {
		serverClaim.Labels = make(map[string]string)
	}
	serverClaim.Labels[LabelKeyClusterName] = o.clusterName
	klog.V(2).InfoS("Adding cluster name label to server claim object", "ServerClaim", client.ObjectKeyFromObject(serverClaim), "Node", node.Name)
	if err := o.metalClient.Patch(ctx, serverClaim, client.MergeFrom(serverClaimBase)); err != nil {
		return nil, fmt.Errorf("failed to patch server claim for Node %s: %w", node.Name, err)
	}

	server := &metalv1alpha1.Server{}
	if err := o.metalClient.Get(ctx, client.ObjectKey{Name: serverClaim.Spec.ServerRef.Name}, server); err != nil {
		return nil, fmt.Errorf("failed to get server object for node %s: %w", node.Name, err)
	}

	// collect internal Node IPs
	addresses := make([]corev1.NodeAddress, 0, len(server.Status.NetworkInterfaces))
	for _, iface := range server.Status.NetworkInterfaces {
		addresses = append(addresses, corev1.NodeAddress{
			Type:    corev1.NodeInternalIP,
			Address: iface.IP.String(),
		})
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		providerID = getProviderIDForServerClaim(serverClaim)
	}

	// TODO: use constants here
	instanceType, ok := server.Labels["instance-type"]
	if !ok {
		klog.V(2).InfoS("No instance type label found for node instance", "Node", node.Name)
	}

	zone, ok := server.Labels["zone"]
	if !ok {
		klog.V(2).InfoS("No zone label found for node instance", "Node", node.Name)
	}

	region, ok := server.Labels["region"]
	if !ok {
		klog.V(2).InfoS("No region label found for node instance", "Node", node.Name)
	}

	return &cloudprovider.InstanceMetadata{
		ProviderID:    providerID,
		InstanceType:  instanceType,
		NodeAddresses: addresses,
		Zone:          zone,
		Region:        region,
	}, nil
}

func getProviderIDForServerClaim(serverClaim *metalv1alpha1.ServerClaim) string {
	return fmt.Sprintf("%s://%s/%s/%s", ProviderName, serverClaim.Namespace, serverClaim.Name, serverClaim.UID)
}

func (o *metalInstancesV2) getServerClaimForNode(ctx context.Context, node *corev1.Node) (*metalv1alpha1.ServerClaim, error) {
	var serverClaim *metalv1alpha1.ServerClaim
	if node.Spec.ProviderID != "" {
		return o.getServerClaimFromProviderID(ctx, node.Spec.ProviderID)
	}

	serverClaimList := &metalv1alpha1.ServerClaimList{}
	if err := o.metalClient.List(ctx, serverClaimList, client.InNamespace(o.metalNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list server claims for node %s: %w", node.Name, err)
	}

	for _, claim := range serverClaimList.Items {
		server := &metalv1alpha1.Server{}
		if err := o.metalClient.Get(ctx, client.ObjectKey{Name: claim.Spec.ServerRef.Name}, server); err != nil {
			return nil, fmt.Errorf("failed to get server object for node %s: %w", node.Name, err)
		}
		//Avoid case mismatch by converting to lower case
		if nodeInfo := node.Status.NodeInfo; nodeInfo.SystemUUID == strings.ToLower(server.Spec.UUID) {
			return &claim, nil
		}
	}

	return serverClaim, nil
}

func (o *metalInstancesV2) getServerClaimFromProviderID(ctx context.Context, providerID string) (*metalv1alpha1.ServerClaim, error) {
	objKey, objUID, err := getObjectKeyFromProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get object key for ProviderID %s: %w", providerID, err)
	}
	serverClaim := &metalv1alpha1.ServerClaim{}
	if err := o.metalClient.Get(ctx, objKey, serverClaim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloudprovider.InstanceNotFound
		}
		return nil, fmt.Errorf("failed to get server claim object for ProviderID %s: %w", providerID, err)
	}

	// Check for UID equality. For backwards compatibility we only compare if the UID is part of the ProviderID.
	if len(objUID) > 0 && serverClaim.UID != objUID {
		return nil, cloudprovider.InstanceNotFound
	}

	return serverClaim, nil
}

func getObjectKeyFromProviderID(providerID string) (client.ObjectKey, types.UID, error) {
	parts := strings.Split(strings.TrimPrefix(providerID, fmt.Sprintf("%s://", ProviderName)), "/")
	if len(parts) != 3 {
		return client.ObjectKey{}, "", fmt.Errorf("invalid format of ProviderID %s", providerID)
	}
	return client.ObjectKey{
		Namespace: parts[0],
		Name:      parts[1],
	}, types.UID(parts[2]), nil
}
