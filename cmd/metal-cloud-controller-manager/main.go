// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/ironcore-dev/cloud-provider-metal/pkg/cloudprovider/metal"
	"github.com/ironcore-dev/cloud-provider-metal/pkg/version"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	cloudcontrollerconfig "k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/clientgo"
	_ "k8s.io/component-base/metrics/prometheus/version"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	ctrl.SetLogger(klog.NewKlogr())

	ccmOptions, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		klog.ErrorS(err, "unable to initialize command options")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	fss := cliflag.NamedFlagSets{}
	metal.AddExtraFlags(fss.FlagSet("Metal Client"))

	command := app.NewCloudControllerManagerCommand(ccmOptions,
		cloudInitializer,
		app.DefaultInitFuncConstructors,
		names.CCMControllerAliases(),
		fss,
		wait.NeverStop)

	klog.V(1).InfoS("metal-cloud-controller-manager version", "version", version.Version)

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cloudInitializer(config *cloudcontrollerconfig.CompletedConfig) cloudprovider.Interface {
	cloudConfig := config.ComponentConfig.KubeCloudShared.CloudProvider
	providerName := cloudConfig.Name

	if providerName == "" {
		providerName = metal.ProviderName
	}

	// initialize cloud provider with the cloud provider name and config file provided
	cloud, err := cloudprovider.InitCloudProvider(providerName, cloudConfig.CloudConfigFile)
	if err != nil {
		klog.ErrorS(err, "cloud provider could not be initialized")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	if cloud == nil {
		klog.ErrorS(nil, "cloud provider is nil")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	if cloud != nil && !cloud.HasClusterID() {
		if config.ComponentConfig.KubeCloudShared.AllowUntaggedCloud {
			klog.InfoS("WARNING: detected a cluster without a ClusterID", "detail",
				"A ClusterID will be required in the future. Please tag your cluster to avoid any future issues.")
		} else {
			klog.ErrorS(nil, "No ClusterID found, a ClusterID is required for the cloud provider to function properly, "+
				"this check can be bypassed by setting the allow-untagged-cloud option")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}

	return cloud
}
