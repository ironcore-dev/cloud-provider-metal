// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("ServerClaimReconciler", func() {

	var (
		serverClaim *metalv1alpha1.ServerClaim
		node        *corev1.Node
	)

	ns, cp, _ := SetupTest(CloudConfig{
		ClusterName: "test-cluster",
	})

	BeforeEach(func(ctx SpecContext) {
		var ok bool
		instancesProvider, ok = (*cp).InstancesV2()
		Expect(ok).To(BeTrue())

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
		serverClaim = &metalv1alpha1.ServerClaim{
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
		node = &corev1.Node{
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

		By("Ensuring that the Node has a provider ID")
		meta, err := instancesProvider.InstanceMetadata(ctx, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta).NotTo(BeNil())

		originalNode := node.DeepCopy()
		node.Spec.ProviderID = meta.ProviderID
		Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())
	})

	It("should not change the labels of an operational node", func(ctx SpecContext) {
		originalNode := node.DeepCopy()
		testLabels := map[string]string{
			"test-label": "test-value",
		}
		node.Labels = testLabels
		Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())
		Consistently(Object(node)).Should(HaveField("Labels", testLabels))
	})

	It("should copy over the maintenance needed label", func(ctx SpecContext) {
		originalServerClaim := serverClaim.DeepCopy()
		serverClaim.Labels = map[string]string{
			metalv1alpha1.ServerMaintenanceNeededLabelKey: "true",
		}
		Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalServerClaim))).To(Succeed())

		Eventually(Object(node)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceNeededLabelKey, "true")))
		Consistently(Object(node)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceNeededLabelKey, "true")))
	})

	It("should remove the maintenance needed label when not needed", func(ctx SpecContext) {
		originalNode := node.DeepCopy()
		node.Labels = map[string]string{
			metalv1alpha1.ServerMaintenanceNeededLabelKey: "true",
		}
		Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())
		Eventually(Object(node)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceNeededLabelKey, "true")))

		// Trigger the reconciler to remove the label
		originalServerClaim := serverClaim.DeepCopy()
		serverClaim.Labels = map[string]string{"a": "b"}
		Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalServerClaim))).To(Succeed())

		Eventually(Object(node)).Should(HaveField("Labels", BeEmpty()))
		Consistently(Object(node)).Should(HaveField("Labels", BeEmpty()))
	})
})
