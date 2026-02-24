// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"fmt"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("NodeMaintenanceReconciler", func() {

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
		}
		Expect(k8sClient.Create(ctx, server)).To(Succeed())
		DeferCleanup(k8sClient.Delete, server)

		By("Creating a ServerClaim for a Node")
		serverClaim = &metalv1alpha1.ServerClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns.Name,
				GenerateName: "test-",
			},
			Spec: metalv1alpha1.ServerClaimSpec{
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
		DeferCleanup(func(ctx SpecContext) error {
			return client.IgnoreNotFound(k8sClient.Delete(ctx, node))
		})

		originalNode := node.DeepCopy()
		node.Spec.ProviderID = fmt.Sprintf("metal://%s/%s", serverClaim.Namespace, serverClaim.Name)
		Expect(k8sClient.Patch(ctx, node, client.MergeFrom(originalNode))).To(Succeed())
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
			Expect(maintenanceCR.Spec.Priority).To(Equal(int32(100)))
			Expect(maintenanceCR.Spec.ServerRef).NotTo(BeNil())
			Expect(maintenanceCR.Spec.ServerRef.Name).To(Equal(serverClaim.Spec.ServerRef.Name))

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
					Finalizers: []string{nodeMaintenanceFinalizer},
				},
				Spec: metalv1alpha1.ServerMaintenanceSpec{
					Policy:   metalv1alpha1.ServerMaintenancePolicyOwnerApproval,
					Priority: 100,
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
			Consistently(Object(node)).Should(HaveField("Finalizers", ContainElement(nodeMaintenanceFinalizer)))
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
