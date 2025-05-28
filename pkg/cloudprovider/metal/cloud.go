// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"fmt"
	"io"
	"log"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	capiv1beta1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

const (
	// ProviderName is the name of the cloud provider
	ProviderName = "metal"
	// serverClaimMetadataUIDField is the field used to index ServerClaims by their UID
	serverClaimMetadataUIDField = ".metadata.uid"
	// LoopbackAddressAnnotation is the annotation used to specify a loopback address for the Machine
	LoopbackAddressAnnotation = "metal.ironcore.dev/loopback-address"
)

var metalScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(metalv1alpha1.AddToScheme(metalScheme))
	utilruntime.Must(capiv1beta1.AddToScheme(metalScheme))

	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		cfg, err := LoadCloudProviderConfig(config)
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode config")
		}

		metalCluster, err := cluster.New(cfg.RestConfig, func(o *cluster.Options) {
			o.Scheme = metalScheme
			o.Cache.DefaultNamespaces = map[string]cache.Config{
				cfg.Namespace: {},
			}
		})
		if err != nil {
			return nil, fmt.Errorf("unable to create metal cluster: %w", err)
		}

		return &cloud{
			metalCluster:   metalCluster,
			metalNamespace: cfg.Namespace,
			cloudConfig:    cfg.cloudConfig,
		}, nil
	})
}

type cloud struct {
	targetCluster  cluster.Cluster
	metalCluster   cluster.Cluster
	metalNamespace string
	cloudConfig    CloudConfig
	instancesV2    cloudprovider.InstancesV2
}

func (o *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	klog.V(2).Infof("Initializing cloud provider: %s", ProviderName)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		<-stop
	}()

	cfg, err := clientBuilder.Config("cloud-controller-manager")
	if err != nil {
		log.Fatalf("Failed to get config: %v", err)
	}
	o.targetCluster, err = cluster.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create new cluster: %v", err)
	}

	o.instancesV2 = newMetalInstancesV2(
		o.targetCluster.GetClient(),
		o.metalCluster.GetClient(),
		o.metalNamespace,
		o.cloudConfig,
	)

	if err := o.metalCluster.GetFieldIndexer().IndexField(ctx, &metalv1alpha1.ServerClaim{}, serverClaimMetadataUIDField, func(object client.Object) []string {
		serverClaim := object.(*metalv1alpha1.ServerClaim)
		return []string{string(serverClaim.UID)}
	}); err != nil {
		log.Fatalf("Failed to setup field indexer for server claims: %v", err)
	}

	if _, err := o.targetCluster.GetCache().GetInformer(ctx, &corev1.Node{}); err != nil {
		log.Fatalf("Failed to setup Node informer: %v", err)
	}
	// TODO: setup informer for Services

	go func() {
		if err := o.metalCluster.Start(ctx); err != nil {
			log.Fatalf("Failed to start metal cluster: %v", err)
		}
	}()

	go func() {
		if err := o.targetCluster.Start(ctx); err != nil {
			log.Fatalf("Failed to start target cluster: %v", err)
		}
	}()

	if !o.metalCluster.GetCache().WaitForCacheSync(ctx) {
		log.Fatal("Failed to wait for metal cluster cache to sync")
	}
	if !o.targetCluster.GetCache().WaitForCacheSync(ctx) {
		log.Fatal("Failed to wait for target cluster cache to sync")
	}
	klog.V(2).Infof("Successfully initialized cloud provider: %s", ProviderName)
}

func (o *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return nil, false
}

// Instances returns an implementation of Instances for metal
func (o *cloud) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

// InstancesV2 is an implementation for instances and should only be implemented by external cloud providers.
// Implementing InstancesV2 is behaviorally identical to Instances but is optimized to significantly reduce
// API calls to the cloud provider when registering and syncing nodes.
// Also returns true if the interface is supported, false otherwise.
func (o *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return o.instancesV2, true
}

// Zones returns an implementation of Zones for metal
func (o *cloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns the list of clusters
func (o *cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns an implementation of Routes for metal
func (o *cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID
func (o *cloud) ProviderName() string {
	return ProviderName
}

// HasClusterID returns true if the cluster has a clusterID
func (o *cloud) HasClusterID() bool {
	return true
}
