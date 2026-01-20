// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"net"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"
)

var _ = Describe("NodeReconciler", func() {

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
					metalv1alpha1.AnnotationInstanceType: "foo",
					corev1.LabelTopologyZone:             "a",
					corev1.LabelTopologyRegion:           "bar",
				},
			},
			Spec: metalv1alpha1.ServerSpec{
				SystemUUID: "12345",
				Power:      "On",
			},
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Patching the Server object to have a valid network interface status")
		Eventually(UpdateStatus(server, func() {
			server.Status.PowerState = metalv1alpha1.ServerOnPowerState
			server.Status.NetworkInterfaces = []metalv1alpha1.NetworkInterface{{
				Name: "my-nic",
				IPs:  []metalv1alpha1.IP{metalv1alpha1.MustParseIP("10.0.0.1")},
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

	It("should copy the approval label for a claim requiring maintenance", func(ctx SpecContext) {
		originalServerClaim := serverClaim.DeepCopy()
		serverClaim.Labels = map[string]string{
			metalv1alpha1.ServerMaintenanceNeededLabelKey: TrueStr,
		}
		Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalServerClaim))).To(Succeed())

		Eventually(Object(node)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceNeededLabelKey, TrueStr)))
		Consistently(Object(node)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceNeededLabelKey, TrueStr)))

		originalNode := node.DeepCopy()
		node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr
		Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

		Eventually(Object(serverClaim)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceApprovalKey, TrueStr)))
	})

	Context("PodCIDR assignment", func() {
		BeforeEach(func() {
			PodPrefixSize = 24
		})

		AfterEach(func() {
			PodPrefixSize = 0
		})

		It("should assign PodCIDR to a node with NodeInternalIP", func(ctx SpecContext) {
			By("Setting the NodeInternalIP address on the node")
			Eventually(UpdateStatus(node, func() {
				node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.0.5.42",
				})
			})).Should(Succeed())

			By("Verifying PodCIDR is assigned to the node")
			Eventually(Object(node)).Should(HaveField("Spec.PodCIDR", "10.0.5.0/24"))
			Eventually(Object(node)).Should(HaveField("Spec.PodCIDRs", ContainElement("10.0.5.0/24")))
		})

		It("should not overwrite existing PodCIDR", func(ctx SpecContext) {
			By("Setting the PodCIDR on the node")
			originalNode := node.DeepCopy()
			node.Spec.PodCIDR = "192.168.0.0/24"
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			By("Setting the NodeInternalIP address on the node")
			Eventually(UpdateStatus(node, func() {
				node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.0.5.42",
				})
			})).Should(Succeed())

			By("Verifying PodCIDR was not overwritten")
			Consistently(Object(node)).Should(HaveField("Spec.PodCIDR", "192.168.0.0/24"))
		})

		It("should not assign PodCIDR if node has no NodeInternalIP", func(ctx SpecContext) {
			By("Triggering reconciliation by patching the claim")
			originalServerClaim := serverClaim.DeepCopy()
			serverClaim.Labels = map[string]string{
				metalv1alpha1.ServerMaintenanceNeededLabelKey: TrueStr,
			}
			Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalServerClaim))).To(Succeed())

			By("Verifying PodCIDR remains empty")
			Consistently(Object(node)).Should(HaveField("Spec.PodCIDR", ""))
		})

		It("should not assign PodCIDR if PodPrefixSize is disabled", func(ctx SpecContext) {
			By("Disabling PodCIDR assignment")
			PodPrefixSize = 0

			By("Setting the NodeInternalIP address on the node")
			Eventually(UpdateStatus(node, func() {
				node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{
					Type:    corev1.NodeInternalIP,
					Address: "10.0.5.42",
				})
			})).Should(Succeed())

			By("Triggering reconciliation by patching the claim")
			originalServerClaim := serverClaim.DeepCopy()
			serverClaim.Labels = map[string]string{
				metalv1alpha1.ServerMaintenanceNeededLabelKey: TrueStr,
			}
			Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalServerClaim))).To(Succeed())

			By("Verifying PodCIDR remains empty despite having NodeInternalIP")
			Consistently(Object(node)).Should(HaveField("Spec.PodCIDR", ""))
		})
	})
})

var _ = Describe("zeroHostBits", func() {
	DescribeTable("should correctly mask IPv4 addresses",
		func(ip string, maskSize int, expected string) {
			result := zeroHostBits(net.ParseIP(ip), maskSize)
			Expect(result.String()).To(Equal(expected))
		},
		Entry("mask /24", "10.0.5.42", 24, "10.0.5.0"),
		Entry("mask /16", "10.0.5.42", 16, "10.0.0.0"),
		Entry("mask /8", "10.20.30.40", 8, "10.0.0.0"),
		Entry("mask /32", "10.0.5.42", 32, "10.0.5.42"),
		Entry("mask /0", "10.0.5.42", 0, "0.0.0.0"),
	)

	DescribeTable("should correctly mask IPv6 addresses",
		func(ip string, maskSize int, expected string) {
			result := zeroHostBits(net.ParseIP(ip), maskSize)
			Expect(result.String()).To(Equal(expected))
		},
		Entry("mask /64", "2001:db8::1", 64, "2001:db8::"),
		Entry("mask /48", "2001:db8:1234:5678::1", 48, "2001:db8:1234::"),
		Entry("mask /128", "2001:db8::1", 128, "2001:db8::1"),
		Entry("mask /0", "2001:db8::1", 0, "::"),
	)
})
