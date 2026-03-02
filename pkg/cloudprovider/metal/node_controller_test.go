// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"net"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
		DeferCleanup(func(ctx SpecContext) error {
			return client.IgnoreNotFound(k8sClient.Delete(ctx, serverClaim))
		})

		By("Creating a Node object with a provider ID referencing the machine")
		node = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) error {
			return client.IgnoreNotFound(k8sClient.Delete(ctx, node))
		})

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

	Context("ServerMaintenance CR Lifecycle", func() {
		It("should create a ServerMaintenance CR when maintenance is requested on the Node", func(ctx SpecContext) {
			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			maintenanceCR := &metalv1alpha1.ServerMaintenance{}
			maintenanceKey := client.ObjectKey{
				Namespace: serverClaim.Namespace,
				Name:      serverClaim.Name,
			}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			By("Verifying the ServerMaintenance CR fields")
			Expect(maintenanceCR.Spec.Policy).To(Equal(metalv1alpha1.ServerMaintenancePolicyOwnerApproval))
			Expect(maintenanceCR.Spec.Priority).To(Equal(serverMaintenancePriority))
			Expect(maintenanceCR.Spec.ServerRef).NotTo(BeNil())
			Expect(maintenanceCR.Spec.ServerRef.Name).To(Equal(serverClaim.Spec.ServerRef.Name))
			Expect(maintenanceCR.Labels).To(HaveKeyWithValue(labelKeyManagedBy, cloudProviderMetalName))

			By("Verifying the finalizer is added to the Node")
			Eventually(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})

		It("should do nothing if ServerMaintenance CR already exists (idempotency)", func(ctx SpecContext) {
			existingCR := &metalv1alpha1.ServerMaintenance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverClaim.Name,
					Namespace: serverClaim.Namespace,
					Labels: map[string]string{
						"test-marker": "do-not-overwrite",
					},
				},
				Spec: metalv1alpha1.ServerMaintenanceSpec{
					Policy:   metalv1alpha1.ServerMaintenancePolicyOwnerApproval,
					Priority: serverMaintenancePriority,
					ServerRef: &corev1.LocalObjectReference{
						Name: serverClaim.Spec.ServerRef.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, existingCR)).To(Succeed())
			DeferCleanup(k8sClient.Delete, existingCR)

			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			By("Ensuring the existing CR is completely untouched by the controller")
			Consistently(func(g Gomega) {
				checkCR := &metalv1alpha1.ServerMaintenance{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(existingCR), checkCR)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(checkCR.Labels).To(HaveKeyWithValue("test-marker", "do-not-overwrite"))
			}).Should(Succeed())

			By("Verifying the finalizer is present in the Node")
			Eventually(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
			Consistently(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})

		It("should NOT overwrite an existing ServerMaintenance CR if it is not managed by the controller", func(ctx SpecContext) {
			unownedCR := &metalv1alpha1.ServerMaintenance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverClaim.Name,
					Namespace: serverClaim.Namespace,
					Labels: map[string]string{
						labelKeyManagedBy: "admin-user",
						"custom-marker":   "do-not-touch",
					},
				},
				Spec: metalv1alpha1.ServerMaintenanceSpec{
					Policy:   metalv1alpha1.ServerMaintenancePolicyOwnerApproval,
					Priority: 999,
					ServerRef: &corev1.LocalObjectReference{
						Name: serverClaim.Spec.ServerRef.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, unownedCR)).To(Succeed())
			DeferCleanup(k8sClient.Delete, unownedCR)

			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			Consistently(func(g Gomega) {
				checkCR := &metalv1alpha1.ServerMaintenance{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(unownedCR), checkCR)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(checkCR.Spec.Priority).To(Equal(int32(999)))
				g.Expect(checkCR.Labels).To(HaveKeyWithValue(labelKeyManagedBy, "admin-user"))
				g.Expect(checkCR.Labels).To(HaveKeyWithValue("custom-marker", "do-not-touch"))
			}).Should(Succeed())

			Eventually(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})

		It("should delete the ServerMaintenance CR when the maintenance-requested label is removed", func(ctx SpecContext) {
			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			maintenanceKey := client.ObjectKey{
				Namespace: serverClaim.Namespace,
				Name:      serverClaim.Name,
			}
			maintenanceCR := &metalv1alpha1.ServerMaintenance{}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			originalNodeWithLabel := node.DeepCopy()
			delete(node.Labels, metalv1alpha1.ServerMaintenanceRequestedLabelKey)
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNodeWithLabel))).To(Succeed())

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())

			By("Verifying the finalizer is removed from the Node")
			Eventually(Object(node)).ShouldNot(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})

		It("should do nothing if the label is absent and CR does not exist (idempotency)", func(ctx SpecContext) {
			originalNode := node.DeepCopy()
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			node.Annotations["dummy-trigger"] = "true"
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			maintenanceKey := client.ObjectKey{
				Namespace: serverClaim.Namespace,
				Name:      serverClaim.Name,
			}
			maintenanceCR := &metalv1alpha1.ServerMaintenance{}

			Consistently(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())

			Consistently(Object(node)).ShouldNot(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})

		It("should handle Node deletion by cleaning up the CR and removing the finalizer", func(ctx SpecContext) {
			By("Triggering maintenance to create CR and add finalizer")
			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			maintenanceKey := client.ObjectKey{
				Namespace: serverClaim.Namespace,
				Name:      serverClaim.Name,
			}
			maintenanceCR := &metalv1alpha1.ServerMaintenance{}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())
			Eventually(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))

			By("Deleting the Node")
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			By("Verifying the ServerMaintenance CR is deleted first")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())

			By("Verifying the Node is completely deleted (finalizer was removed)")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), &corev1.Node{})
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())
		})

		It("should NOT delete the ServerMaintenance CR if it is not managed by the controller", func(ctx SpecContext) {
			const managedBy = "some-other-controller-or-admin"
			unownedCR := &metalv1alpha1.ServerMaintenance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serverClaim.Name,
					Namespace: serverClaim.Namespace,
					Labels: map[string]string{
						labelKeyManagedBy: managedBy,
					},
				},
				Spec: metalv1alpha1.ServerMaintenanceSpec{
					Policy:   metalv1alpha1.ServerMaintenancePolicyOwnerApproval,
					Priority: serverMaintenancePriority,
					ServerRef: &corev1.LocalObjectReference{
						Name: serverClaim.Spec.ServerRef.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, unownedCR)).To(Succeed())
			DeferCleanup(k8sClient.Delete, unownedCR)

			originalNode := node.DeepCopy()
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			node.Annotations["dummy-trigger"] = "trigger-reconcile"
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			By("Ensuring the unowned ServerMaintenance CR remains untouched")
			Consistently(func(g Gomega) {
				checkCR := &metalv1alpha1.ServerMaintenance{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(unownedCR), checkCR)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(checkCR.Labels).To(HaveKeyWithValue(labelKeyManagedBy, managedBy))
			}).Should(Succeed())
		})

		It("should clean up ServerMaintenance and finalizer even if ServerClaim is deleted", func(ctx SpecContext) {
			By("1. Triggering maintenance to create CR and add finalizer")
			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceRequestedLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			maintenanceKey := client.ObjectKey{
				Namespace: serverClaim.Namespace,
				Name:      serverClaim.Name,
			}
			maintenanceCR := &metalv1alpha1.ServerMaintenance{}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())
			Eventually(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))

			By("2. Deleting the ServerClaim to simulate the edge case")
			Expect(k8sClient.Delete(ctx, serverClaim)).To(Succeed())
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(serverClaim), &metalv1alpha1.ServerClaim{})
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())

			By("3. Removing the maintenance-requested label from the Node")
			originalNodeWithLabel := node.DeepCopy()
			delete(node.Labels, metalv1alpha1.ServerMaintenanceRequestedLabelKey)
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNodeWithLabel))).To(Succeed())

			By("4. Verifying the ServerMaintenance CR is deleted despite missing ServerClaim")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, maintenanceKey, maintenanceCR)
				g.Expect(err).To(HaveOccurred())
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).Should(Succeed())

			By("5. Verifying the finalizer is removed from the Node")
			Eventually(Object(node)).ShouldNot(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
		})
	})

	Context("Maintenance Approval Handshake", func() {
		It("should add the approval label to the ServerClaim when Node is approved", func(ctx SpecContext) {
			originalClaim := serverClaim.DeepCopy()
			if serverClaim.Labels == nil {
				serverClaim.Labels = make(map[string]string)
			}
			serverClaim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalClaim))).To(Succeed())

			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			By("Verifying the ServerClaim receives the Approval label from the NodeMaintenanceReconciler")
			Eventually(Object(serverClaim)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceApprovalKey, TrueStr)))
		})

		It("should remove the approval label from the ServerClaim when Node loses the approval label", func(ctx SpecContext) {
			originalClaim := serverClaim.DeepCopy()
			if serverClaim.Labels == nil {
				serverClaim.Labels = make(map[string]string)
			}
			serverClaim.Labels[metalv1alpha1.ServerMaintenanceNeededLabelKey] = TrueStr
			Expect(k8sClient.Patch(ctx, serverClaim, client.MergeFrom(originalClaim))).To(Succeed())

			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			Eventually(Object(serverClaim)).Should(HaveField("Labels", HaveKeyWithValue(metalv1alpha1.ServerMaintenanceApprovalKey, TrueStr)))

			originalNode2 := node.DeepCopy()
			delete(node.Labels, metalv1alpha1.ServerMaintenanceApprovalKey)
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode2))).To(Succeed())

			By("Verifying the ServerClaim loses the Approval label")
			Eventually(Object(serverClaim)).ShouldNot(HaveField("Labels", HaveKey(metalv1alpha1.ServerMaintenanceApprovalKey)))
		})

		It("should NOT add the approval label to the ServerClaim if maintenance was not needed", func(ctx SpecContext) {
			originalNode := node.DeepCopy()
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[metalv1alpha1.ServerMaintenanceApprovalKey] = TrueStr
			Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())

			Consistently(func(g Gomega) {
				checkClaim := &metalv1alpha1.ServerClaim{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(serverClaim), checkClaim)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(checkClaim.Labels).NotTo(HaveKey(metalv1alpha1.ServerMaintenanceApprovalKey))
			}).Should(Succeed())
		})
	})
})

var _ = Describe("parseProviderID", func() {
	DescribeTable("should correctly parse provider ID or return an error",
		func(providerID string, expected types.NamespacedName, expectErr bool) {
			result, err := parseProviderID(providerID)

			if expectErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(expected))
			}
		},

		Entry("valid provider ID", "metal://default/node-1", types.NamespacedName{Namespace: "default", Name: "node-1"}, false),
		Entry("empty string", "", types.NamespacedName{}, true),
		Entry("missing scheme", "metal-default/node-1", types.NamespacedName{}, true),
		Entry("missing provider before scheme", "://default/node-1", types.NamespacedName{}, true),
		Entry("missing namespace or name (no slash)", "metal://node-1", types.NamespacedName{}, true),
		Entry("too many slashes", "metal://default/node-1/extra", types.NamespacedName{}, true),
		Entry("empty namespace", "metal:///name", types.NamespacedName{}, true),
		Entry("empty name", "metal://namespace/", types.NamespacedName{}, true),
		Entry("empty name and namespace", "metal:///", types.NamespacedName{}, true),
	)
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
