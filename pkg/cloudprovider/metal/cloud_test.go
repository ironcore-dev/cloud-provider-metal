// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cloud", func() {
	cloudConfig := CloudConfig{
		ClusterName: clusterName,
		Networking: Networking{
			ConfigureNodeAddresses: true,
		},
	}
	_, cp, _ := SetupTest(cloudConfig)

	It("should ensure the correct cloud provider setup", func() {
		Expect((*cp).HasClusterID()).To(BeTrue())

		Expect((*cp).ProviderName()).To(Equal("metal"))

		clusters, ok := (*cp).Clusters()
		Expect(clusters).To(BeNil())
		Expect(ok).To(BeFalse())

		instances, ok := (*cp).Instances()
		Expect(instances).To(BeNil())
		Expect(ok).To(BeFalse())

		zones, ok := (*cp).Zones()
		Expect(zones).To(BeNil())
		Expect(ok).To(BeFalse())
	})
})
