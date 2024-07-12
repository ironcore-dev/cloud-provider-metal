// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"fmt"
	"io"
	"os"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

type CloudProviderConfig struct {
	RestConfig  *rest.Config
	Namespace   string
	cloudConfig CloudConfig
}

type CloudConfig struct {
	ClusterName string `json:"clusterName"`
}

var (
	MetalKubeconfigPath string
)

func AddExtraFlags(fs *pflag.FlagSet) {
	fs.StringVar(&MetalKubeconfigPath, "metal-kubeconfig", "", "Path to the metal cluster kubeconfig.")
}

func LoadCloudProviderConfig(f io.Reader) (*CloudProviderConfig, error) {
	klog.V(2).Infof("Reading configuration for cloud provider: %s", ProviderName)
	configBytes, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read in config")
	}

	cloudConfig := &CloudConfig{}
	if err := yaml.Unmarshal(configBytes, cloudConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cloud config: %w", err)
	}

	if cloudConfig.ClusterName == "" {
		return nil, fmt.Errorf("clusterName missing in cloud config")
	}

	kubeconfigData, err := os.ReadFile(MetalKubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metal kubeconfig %s: %w", MetalKubeconfigPath, err)
	}

	kubeconfig, err := clientcmd.Load(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("unable to read metal cluster kubeconfig: %w", err)
	}
	clientConfig := clientcmd.NewDefaultClientConfig(*kubeconfig, nil)
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("unable to get metal cluster rest config: %w", err)
	}
	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace from metal kubeconfig: %w", err)
	}
	// TODO: empty or unset namespace will be defaulted to the 'default' namespace. We might want to handle this
	// as an error.
	if namespace == "" {
		return nil, fmt.Errorf("got a empty namespace from metal kubeconfig")
	}
	klog.V(2).Infof("Successfully read configuration for cloud provider: %s", ProviderName)

	return &CloudProviderConfig{
		RestConfig:  restConfig,
		Namespace:   namespace,
		cloudConfig: *cloudConfig,
	}, nil
}
