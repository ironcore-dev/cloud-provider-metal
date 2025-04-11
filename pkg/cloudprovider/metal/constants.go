// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

const (
	// AnnotationKeyClusterName is the cluster name annotation key name
	AnnotationKeyClusterName = "cluster-name"
	// AnnotationKeyServiceName is the service name annotation key name
	AnnotationKeyServiceName = "service-name"
	// AnnotationKeyServiceNamespace is the service namespace annotation key name
	AnnotationKeyServiceNamespace = "service-namespace"
	// AnnotationKeyServiceUID is the service UID annotation key name
	AnnotationKeyServiceUID = "service-uid"
	// AnnotationPowerOff can be set to any value to power off a server
	AnnotationPowerOff = "metal.ironcore.dev/power-off"
	// LabelKeyClusterName is the label key name used to identify the cluster name in Kubernetes labels
	LabelKeyClusterName = "kubernetes.io/cluster"
	// LabelKeyServerClaim is the label key name used to identify the server claim in Kubernetes labels
	LabelKeyServerClaim = "metal.ironcore.dev/server-claim"
)
