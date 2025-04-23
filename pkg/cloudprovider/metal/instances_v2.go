// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
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
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	capiv1beta1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type metalInstancesV2 struct {
	targetClient   client.Client
	metalClient    client.Client
	metalNamespace string
	cloudConfig    CloudConfig
}

func newMetalInstancesV2(targetClient client.Client, metalClient client.Client, namespace string, cloudConfig CloudConfig) cloudprovider.InstancesV2 {
	return &metalInstancesV2{
		targetClient:   targetClient,
		metalClient:    metalClient,
		metalNamespace: namespace,
		cloudConfig:    cloudConfig,
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
	serverClaim.Labels[LabelKeyClusterName] = o.cloudConfig.ClusterName
	klog.V(2).InfoS("Adding cluster name label to server claim object", "ServerClaim", client.ObjectKeyFromObject(serverClaim), "Node", node.Name)
	if err := o.metalClient.Patch(ctx, serverClaim, client.MergeFrom(serverClaimBase)); err != nil {
		return nil, fmt.Errorf("failed to patch server claim for Node %s: %w", node.Name, err)
	}

	if err = o.setServerClaimPower(ctx, node, serverClaim); err != nil {
		return nil, err
	}

	server := &metalv1alpha1.Server{}
	if err := o.metalClient.Get(ctx, client.ObjectKey{Name: serverClaim.Spec.ServerRef.Name}, server); err != nil {
		return nil, fmt.Errorf("failed to get server object for node %s: %w", node.Name, err)
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		providerID = fmt.Sprintf("%s://%s/%s", ProviderName, o.metalNamespace, serverClaim.Name)
	}

	instanceType, ok := server.Labels[metalv1alpha1.InstanceTypeAnnotation]
	if !ok {
		klog.V(2).InfoS("No instance type label found for node instance", "Node", node.Name)
	}

	zone, ok := server.Labels[corev1.LabelTopologyZone]
	if !ok {
		klog.V(2).InfoS("No zone label found for node instance", "Node", node.Name)
	}

	region, ok := server.Labels[corev1.LabelTopologyRegion]
	if !ok {
		klog.V(2).InfoS("No region label found for node instance", "Node", node.Name)
	}

	klog.V(2).InfoS("Additional labels for node instance", "Node", node.Name, "Labels", server.Labels)

	metaData := &cloudprovider.InstanceMetadata{
		ProviderID:       providerID,
		InstanceType:     instanceType,
		Zone:             zone,
		Region:           region,
		AdditionalLabels: server.Labels,
	}

	if metaData.NodeAddresses, err = o.getNodeAddresses(ctx, server, serverClaim); err != nil {
		return nil, err
	}
	return metaData, nil
}

func (o *metalInstancesV2) getNodeAddresses(ctx context.Context, server *metalv1alpha1.Server, claim *metalv1alpha1.ServerClaim) ([]corev1.NodeAddress, error) {
	addresses := make([]corev1.NodeAddress, 0)
	if !o.cloudConfig.Networking.ConfigureNodeAddresses {
		return addresses, nil
	}
	if o.cloudConfig.Networking.IPAMKind == nil {
		for _, iface := range server.Status.NetworkInterfaces {
			addresses = append(addresses, corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: iface.IP.String(),
			})
		}
		return addresses, nil
	}
	ipamKind := o.cloudConfig.Networking.IPAMKind
	if ipamKind.APIGroup == capiv1beta1.GroupVersion.Group && ipamKind.Kind == "IPAddress" {
		selector := client.MatchingLabels{
			LabelKeyServerClaimName:      claim.Name,
			LabelKeyServerClaimNamespace: claim.Namespace,
		}
		var allIpClaims capiv1beta1.IPAddressClaimList
		if err := o.metalClient.List(ctx, &allIpClaims, client.InNamespace(o.metalNamespace), selector); err != nil {
			return nil, err
		}
		for _, ipClaim := range allIpClaims.Items {
			if ipClaim.Status.AddressRef.Name == "" {
				continue
			}
			var ipAddr capiv1beta1.IPAddress
			if err := o.metalClient.Get(ctx, client.ObjectKey{Name: ipClaim.Status.AddressRef.Name, Namespace: ipClaim.Namespace}, &ipAddr); err != nil {
				return nil, fmt.Errorf("failed to get ip address object for node %s: %w", claim.Name, err)
			}
			addresses = append(addresses, corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: ipAddr.Spec.Address,
			})
		}
		return addresses, nil
	}
	return nil, errors.New("unknown ipamKind used for node ip address assignment")
}

// setServerClaimPower ensures that the server claim:
// - is powered off if the node has the powerOffAnnotation and
// - is powered on if the node does not have the powerOffAnnotation
// This does not guarantee that other controllers such as the
// machine-controller-manager interfere with the power state of the server claim.
func (o *metalInstancesV2) setServerClaimPower(ctx context.Context, node *corev1.Node, serverClaim *metalv1alpha1.ServerClaim) error {
	_, powerOff := node.Annotations[AnnotationPowerOff]
	if powerOff && serverClaim.Spec.Power != metalv1alpha1.PowerOff {
		klog.InfoS("Ensuring server is powered off", "Node", node.Name)
		serverClaimBase := serverClaim.DeepCopy()
		serverClaim.Spec.Power = metalv1alpha1.PowerOff
		if err := o.metalClient.Patch(ctx, serverClaim, client.MergeFrom(serverClaimBase)); err != nil {
			return fmt.Errorf("failed to patch server claim for Node %s: %w", node.Name, err)
		}
	}
	if !powerOff && serverClaim.Spec.Power == metalv1alpha1.PowerOff {
		klog.InfoS("Ensuring server is powered on", "Node", node.Name)
		serverClaimBase := serverClaim.DeepCopy()
		serverClaim.Spec.Power = metalv1alpha1.PowerOn
		if err := o.metalClient.Patch(ctx, serverClaim, client.MergeFrom(serverClaimBase)); err != nil {
			return fmt.Errorf("failed to patch server claim for Node %s: %w", node.Name, err)
		}
	}
	return nil
}

func (o *metalInstancesV2) getServerClaimForNode(ctx context.Context, node *corev1.Node) (*metalv1alpha1.ServerClaim, error) {
	if node.Spec.ProviderID != "" {
		return o.getServerClaimFromProviderID(ctx, node.Spec.ProviderID)
	}

	serverClaimList := &metalv1alpha1.ServerClaimList{}
	if err := o.metalClient.List(ctx, serverClaimList, client.InNamespace(o.metalNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list server claims for node %s: %w", node.Name, err)
	}

	for _, claim := range serverClaimList.Items {
		if claim.Spec.ServerRef == nil {
			continue
		}
		server := &metalv1alpha1.Server{}
		if err := o.metalClient.Get(ctx, client.ObjectKey{Name: claim.Spec.ServerRef.Name}, server); err != nil {
			return nil, fmt.Errorf("failed to get server object for node %s: %w", node.Name, err)
		}
		//Avoid case mismatch by converting to lower case
		if nodeInfo := node.Status.NodeInfo; nodeInfo.SystemUUID == strings.ToLower(server.Spec.UUID) {
			return &claim, nil
		}
	}
	return nil, nil
}

func (o *metalInstancesV2) getServerClaimFromProviderID(ctx context.Context, providerID string) (*metalv1alpha1.ServerClaim, error) {
	objKey, err := getObjectKeyFromProviderID(providerID)
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
	return serverClaim, nil
}

func getObjectKeyFromProviderID(providerID string) (client.ObjectKey, error) {
	parts := strings.Split(strings.TrimPrefix(providerID, fmt.Sprintf("%s://", ProviderName)), "/")
	if len(parts) != 2 {
		return client.ObjectKey{}, fmt.Errorf("invalid format of ProviderID %s", providerID)
	}
	return client.ObjectKey{
		Namespace: parts[0],
		Name:      parts[1],
	}, nil
}
