// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"fmt"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudprovider "k8s.io/cloud-provider"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"
)

var (
	instancesProvider cloudprovider.InstancesV2
	clusterName       = "test"
)

var _ = Describe("InstancesV2", func() {
	cloudConfig := CloudConfig{
		ClusterName: clusterName,
		Networking: Networking{
			ConfigureNodeAddresses: true,
		},
	}
	ns, cp, clusterName := SetupTest(cloudConfig)

	BeforeEach(func() {
		By("Instantiating the instances v2 provider")
		var ok bool
		instancesProvider, ok = (*cp).InstancesV2()
		Expect(ok).To(BeTrue())
	})

	It("Should get instance info", func(ctx SpecContext) {
		By("Creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Labels: map[string]string{
					metalv1alpha1.InstanceTypeAnnotation: "foo",
					corev1.LabelTopologyZone:             "a",
					corev1.LabelTopologyRegion:           "bar",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "12345",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IP:   metalv1alpha1.MustParseIP("10.0.0.1"),
			}}
		})).Should(Succeed())

		By("Creating a ServerClaim for a Node")
		serverClaim := &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
				Power:     "On",
				ServerRef: &corev1.LocalObjectReference{Name: server.Name},
			},
		}
		Expect(k8sClient.Create(ctx, serverClaim)).To(Succeed())
		DeferCleanup(k8sClient.Delete, serverClaim)

		By("Creating a Node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Updating the SystemUUID in Node status")
		Eventually(UpdateStatus(node, func() {
			node.Status.NodeInfo.SystemUUID = "12345"
		})).Should(Succeed())

		By("Ensuring that an instance for a Node exists")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("Ensuring that the instance is not shut down")
		ok, err = instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		By("Ensuring that the instance meta data has the correct addresses")
		instanceMetadata, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Eventually(instanceMetadata).Should(SatisfyAll(
			HaveField("ProviderID", getProviderID(serverClaim.Namespace, serverClaim.Name)),
			HaveField("InstanceType", "foo"),
			HaveField("NodeAddresses", ContainElements(
				corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.0.0.1",
				},
			)),
			HaveField("Zone", "a"),
			HaveField("Region", "bar")))

		By("Ensuring cluster name label is added to ServerClaim object")
		Eventually(Object(serverClaim)).Should(SatisfyAll(
			HaveField("Labels", map[string]string{LabelKeyClusterName: clusterName}),
		))
	})

	It("Should get instance info with loopback address", func(ctx SpecContext) {
		By("Creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Annotations: map[string]string{
					LoopbackAddressAnnotation: "10.10.10.10",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "12345",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IP:   metalv1alpha1.MustParseIP("10.0.0.1"),
			}}
		})).Should(Succeed())

		By("Creating a ServerClaim for a Node")
		serverClaim := &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
				Power:     "On",
				ServerRef: &corev1.LocalObjectReference{Name: server.Name},
			},
		}
		Expect(k8sClient.Create(ctx, serverClaim)).To(Succeed())
		DeferCleanup(k8sClient.Delete, serverClaim)

		By("Creating a Node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Updating the SystemUUID in Node status")
		Eventually(UpdateStatus(node, func() {
			node.Status.NodeInfo.SystemUUID = "12345"
		})).Should(Succeed())

		By("Ensuring that the instance meta data has the correct addresses")
		instanceMetadata, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Eventually(instanceMetadata).Should(SatisfyAll(
			HaveField("ProviderID", getProviderID(serverClaim.Namespace, serverClaim.Name)),
			HaveField("NodeAddresses", ContainElements(
				corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.10.10.10",
				},
			)),
		))
	})

	It("Should power off an annotated server", func(ctx SpecContext) {
		By("Creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "9876",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
		})).Should(Succeed())

		By("Creating a ServerClaim for a Node")
		serverClaim := &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
				Power:     "On",
				ServerRef: &corev1.LocalObjectReference{Name: server.Name},
			},
		}
		Expect(k8sClient.Create(ctx, serverClaim)).To(Succeed())
		DeferCleanup(k8sClient.Delete, serverClaim)

		By("Creating a Node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Updating the SystemUUID in Node status")
		Eventually(UpdateStatus(node, func() {
			node.Status.NodeInfo.SystemUUID = "9876"
		})).Should(Succeed())

		By("Ensuring that an instance for a Node exists")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("Ensuring that the instance is not shut down")
		ok, err = instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		By("Ensuring that the instance meta data has the correct providerID")
		instanceMetadata, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Eventually(instanceMetadata).Should(SatisfyAll(
			HaveField("ProviderID", getProviderID(serverClaim.Namespace, serverClaim.Name)),
		))

		By("Ensuring cluster name label is added to ServerClaim object")
		Eventually(Object(serverClaim)).Should(HaveField("Spec.Power", metalv1alpha1.PowerOn))

		By("Annotating the node with power off")
		Eventually(Update(node, func() {
			node.Annotations = map[string]string{
				AnnotationPowerOff: "true",
			}
		})).Should(Succeed())
		_, err = instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).To(Succeed())

		By("Ensuring the ServerClaim is updated to power off")
		Eventually(Object(serverClaim)).Should(HaveField("Spec.Power", metalv1alpha1.PowerOff))

		By("Removing the power off annotation")
		Eventually(Update(node, func() {
			delete(node.Annotations, AnnotationPowerOff)
		})).Should(Succeed())
		_, err = instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).To(Succeed())

		By("Ensuring the ServerClaim is updated to power on")
		Eventually(Object(serverClaim)).Should(HaveField("Spec.Power", metalv1alpha1.PowerOn))
	})

	It("Should get instance info for a Node with correct ProviderID", func(ctx SpecContext) {
		By("Creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Labels: map[string]string{
					metalv1alpha1.InstanceTypeAnnotation: "foo",
					corev1.LabelTopologyZone:             "a",
					corev1.LabelTopologyRegion:           "bar",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "12345",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IP:   metalv1alpha1.MustParseIP("10.0.0.1"),
			}}
		})).Should(Succeed())

		By("Creating a ServerClaim for a Node")
		serverClaim := &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
				Power:     "On",
				ServerRef: &corev1.LocalObjectReference{Name: server.Name},
			},
		}
		Expect(k8sClient.Create(ctx, serverClaim)).To(Succeed())
		DeferCleanup(k8sClient.Delete, serverClaim)

		By("Creating a Node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
			Spec: corev1.NodeSpec{
				ProviderID: getProviderID(serverClaim.Namespace, serverClaim.Name),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Ensuring that an instance for a Node exists")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("Ensuring that the instance is not shut down")
		ok, err = instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		By("Ensuring that the instance meta data has the correct addresses")
		instanceMetadata, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Eventually(instanceMetadata).Should(SatisfyAll(
			HaveField("ProviderID", getProviderID(serverClaim.Namespace, serverClaim.Name)),
			HaveField("InstanceType", "foo"),
			HaveField("NodeAddresses", ContainElements(
				corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.0.0.1",
				},
			)),
			HaveField("Zone", "a"),
			HaveField("Region", "bar")))

		By("Ensuring cluster name label is added to ServerClaim object")
		Eventually(Object(serverClaim)).Should(SatisfyAll(
			HaveField("Labels", map[string]string{LabelKeyClusterName: clusterName}),
		))
	})

	It("Should get InstanceNotFound if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("Creating a Node object with a provider ID referencing non existing ServerClaim")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: corev1.NodeSpec{
				ProviderID: getProviderID(ns.Name, "bar"),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Ensuring that an instance for a Node does not exist")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).To(Equal(cloudprovider.InstanceNotFound))
		Expect(ok).To(BeFalse())
	})

	It("Should fail to get instance metadata if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("Creating a node object with a provider ID referencing non existing ServerClaim")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Ensuring to fail getting the instance metadata")
		metaData, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).To(Equal(cloudprovider.InstanceNotFound))
		Expect(metaData).To(BeNil())
	})

	It("Should fail to get instance shutdown state if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("Creating a Node object with a provider ID referencing non existing ServerClaim")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("ensuring the shutdown state of a node")
		ok, err := instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).To(Equal(cloudprovider.InstanceNotFound))
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("InstancesV2 with configure node addresses false", func() {
	cloudConfig := CloudConfig{
		ClusterName: clusterName,
		Networking: Networking{
			ConfigureNodeAddresses: false,
		},
	}
	ns, cp, clusterName := SetupTest(cloudConfig)

	BeforeEach(func() {
		By("Instantiating the instances v2 provider")
		var ok bool
		instancesProvider, ok = (*cp).InstancesV2()
		Expect(ok).To(BeTrue())
	})

	It("Should not configure node addresses", func(ctx SpecContext) {
		By("Creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Labels: map[string]string{
					metalv1alpha1.InstanceTypeAnnotation: "foo",
					corev1.LabelTopologyZone:             "a",
					corev1.LabelTopologyRegion:           "bar",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "12345",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IP:   metalv1alpha1.MustParseIP("10.0.0.1"),
			}}
		})).Should(Succeed())

		By("Creating a ServerClaim for a Node")
		serverClaim := &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
				Power:     "On",
				ServerRef: &corev1.LocalObjectReference{Name: server.Name},
			},
		}
		Expect(k8sClient.Create(ctx, serverClaim)).To(Succeed())
		DeferCleanup(k8sClient.Delete, serverClaim)

		By("Creating a Node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
			Spec: corev1.NodeSpec{
				ProviderID: getProviderID(serverClaim.Namespace, serverClaim.Name),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("Ensuring that an instance for a Node exists")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("Ensuring that the instance is not shut down")
		ok, err = instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		By("Ensuring that the instance meta data has empty addresses")
		instanceMetadata, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Eventually(instanceMetadata).Should(SatisfyAll(
			HaveField("ProviderID", getProviderID(serverClaim.Namespace, serverClaim.Name)),
			HaveField("InstanceType", "foo"),
			HaveField("NodeAddresses", BeEmpty()),
			HaveField("Zone", "a"),
			HaveField("Region", "bar")))

		By("Ensuring cluster name label is added to ServerClaim object")
		Eventually(Object(serverClaim)).Should(SatisfyAll(
			HaveField("Labels", map[string]string{LabelKeyClusterName: clusterName}),
		))
	})
})

func getProviderID(namespace, serverClaimName string) string {
	return fmt.Sprintf("%s://%s/%s", ProviderName, namespace, serverClaimName)
}
