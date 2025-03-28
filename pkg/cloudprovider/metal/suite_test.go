// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package metal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ironcore-dev/controller-utils/modutils"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/controller-manager/pkg/clientbuilder"
	capiv1beta1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
)

const (
	pollingInterval      = 50 * time.Millisecond
	eventuallyTimeout    = 3 * time.Second
	consistentlyDuration = 1 * time.Second
)

func TestAPIs(t *testing.T) {
	SetDefaultConsistentlyPollingInterval(pollingInterval)
	SetDefaultEventuallyPollingInterval(pollingInterval)
	SetDefaultEventuallyTimeout(eventuallyTimeout)
	SetDefaultConsistentlyDuration(consistentlyDuration)

	RegisterFailHandler(Fail)

	RunSpecs(t, "Cloud Provider Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			modutils.Dir("github.com/ironcore-dev/metal-operator", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,

		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without call the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	DeferCleanup(testEnv.Stop)

	Expect(metalv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(capiv1beta1.AddToScheme(scheme.Scheme)).To(Succeed())

	//+kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// set komega client
	SetClient(k8sClient)
})

func SetupTest(cloudConfig CloudConfig) (*corev1.Namespace, *cloudprovider.Interface, string) {
	var (
		ns = &corev1.Namespace{}
		cp cloudprovider.Interface
	)

	BeforeEach(func(ctx SpecContext) {
		*ns = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "testns-"},
		}
		Expect(k8sClient.Create(ctx, ns)).NotTo(HaveOccurred(), "failed to create test namespace")
		DeferCleanup(k8sClient.Delete, ns)

		user, err := testEnv.AddUser(envtest.User{
			Name:   "dummy",
			Groups: []string{"system:authenticated", "system:masters"},
		}, nil)
		Expect(err).NotTo(HaveOccurred())

		kubeconfigData, err := user.KubeConfig()
		Expect(err).NotTo(HaveOccurred())

		clientConfig, err := clientcmd.Load(kubeconfigData)
		Expect(err).NotTo(HaveOccurred())
		clientConfig.Contexts[clientConfig.CurrentContext].Namespace = ns.Name

		namespacedKubeconfigData, err := clientcmd.Write(*clientConfig)
		Expect(err).NotTo(HaveOccurred())

		kubeconfigFile, err := os.CreateTemp(GinkgoT().TempDir(), "kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_ = kubeconfigFile.Close()
		}()
		Expect(os.WriteFile(kubeconfigFile.Name(), namespacedKubeconfigData, 0666)).To(Succeed())

		curr := MetalKubeconfigPath
		defer func() {
			MetalKubeconfigPath = curr
		}()
		MetalKubeconfigPath = kubeconfigFile.Name()

		cloudConfigFile, err := os.CreateTemp(GinkgoT().TempDir(), "cloud.yaml")
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_ = cloudConfigFile.Close()
		}()
		cloudConfigData, err := yaml.Marshal(&cloudConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(cloudConfigFile.Name(), cloudConfigData, 0666)).To(Succeed())

		cloudProviderCtx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)

		k8sClientSet, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		clientBuilder := clientbuilder.NewDynamicClientBuilder(testEnv.Config, k8sClientSet.CoreV1(), ns.Name)
		cp, err = cloudprovider.InitCloudProvider(ProviderName, cloudConfigFile.Name())
		Expect(err).NotTo(HaveOccurred())
		cp.Initialize(clientBuilder, cloudProviderCtx.Done())
	})

	return ns, &cp, cloudConfig.ClusterName
}
