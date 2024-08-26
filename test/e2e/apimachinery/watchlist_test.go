package apimachinery

import (
	"context"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientfeatures "k8s.io/client-go/features"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"
)

func TestWatchList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, featuregate.Feature(clientfeatures.WatchListClient), false)
	kubeconfig := "/Users/lszaszki/.kube/config"

	// Load the kubeconfig file
	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	framework.ExpectNoError(err)

	ctx := context.Background()

	secretList, err := dynamicClient.Resource(v1.SchemeGroupVersion.WithResource("secrets")).Namespace("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	t.Log(len(secretList.Items))
}

func TestTypedWatchList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, featuregate.Feature(clientfeatures.WatchListClient), false)
	kubeconfig := "/Users/lszaszki/.kube/config"

	// Load the kubeconfig file
	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	framework.ExpectNoError(err)

	ctx := context.Background()

	secretList, err := client.CoreV1().Secrets("kube-namespace").List(ctx, metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	t.Log(len(secretList.Items))
}
