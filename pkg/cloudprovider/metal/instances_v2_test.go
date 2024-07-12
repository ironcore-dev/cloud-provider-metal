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

var _ = Describe("InstancesV2", func() {
	var (
		instancesProvider cloudprovider.InstancesV2
	)
	ns, cp, clusterName := SetupTest()

	It("should get instance info", func(ctx SpecContext) {
		By("instantiating the instances v2 provider")
		var ok bool
		instancesProvider, ok = (*cp).InstancesV2()
		Expect(ok).To(BeTrue())

		By("creating a Server")
		server := &metalv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Labels: map[string]string{
					"instance-type": "foo",
					"zone":          "a",
					"region":        "bar",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				UUID:  "12345",
				Power: "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IP:   metalv1alpha1.MustParseIP("10.0.0.1"),
			}}
		})).Should(Succeed())

		By("creating a ServerClaim for a Node")
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

		By("creating a node object with a provider ID referencing the machine")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: serverClaim.Name,
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("ensuring that an instance for a node exists")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("ensuring that the instance is not shut down")
		ok, err = instancesProvider.InstanceShutdown(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		By("ensuring that the instance meta data has the correct addresses")
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

		By("ensuring cluster name label is added to ServerClaim object")
		Eventually(Object(serverClaim)).Should(SatisfyAll(
			HaveField("Labels", map[string]string{LabelKeyClusterName: clusterName}),
		))
	})

	It("should get InstanceNotFound if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("creating a node object with a provider ID referencing non existing ServerClaim")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("ensuring that an instance for a node does not exist")
		ok, err := instancesProvider.InstanceExists(ctx, node)
		Expect(err).To(Equal(cloudprovider.InstanceNotFound))
		Expect(ok).To(BeFalse())
	})

	It("should fail to get instance metadata if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("creating a node object with a provider ID referencing non existing ServerClaim")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(k8sClient.Delete, node)

		By("ensuring to fail getting the instance metadata")
		metaData, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).To(Equal(cloudprovider.InstanceNotFound))
		Expect(metaData).To(BeNil())
	})

	It("should fail to get instance shutdown state if no ServerClaim exists for Node", func(ctx SpecContext) {
		By("creating a node object with a provider ID referencing non existing ServerClaim")
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

func getProviderID(namespace, serverClaimName string) string {
	return fmt.Sprintf("%s://%s/%s", ProviderName, namespace, serverClaimName)
}
