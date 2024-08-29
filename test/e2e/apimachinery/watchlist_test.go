package apimachinery

import (
	"context"
	"fmt"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	clientfeatures "k8s.io/client-go/features"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/test/e2e/framework"
)

func TestDynamicWatchList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, featuregate.Feature(clientfeatures.WatchListClient), true)
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
	t.Logf("secretList: %+v\n", secretList)
	if len(secretList.Items) != 1 {
		t.Errorf("%#v", secretList.Items)
	}
}

func TestTypedWatchList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, featuregate.Feature(clientfeatures.WatchListClient), true)
	kubeconfig := "/Users/lszaszki/.kube/config"

	// Load the kubeconfig file
	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	framework.ExpectNoError(err)

	ctx := context.Background()

	secretList, err := client.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	if len(secretList.Items) != 1 {
		t.Errorf("%#v", secretList.Items)
	}
}

func TestDynamicWatchListAsTable(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, featuregate.Feature(clientfeatures.WatchListClient), true)

	kubeconfig := "/Users/lszaszki/.kube/config"
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}

	config = dynamic.ConfigFor(config)

	//config.GroupVersion = &schema.GroupVersion{}
	config.AcceptContentTypes = strings.Join([]string{
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1.SchemeGroupVersion.Version, metav1.GroupName),
		fmt.Sprintf("application/json;as=Table;v=%s;g=%s", metav1beta1.SchemeGroupVersion.Version, metav1beta1.GroupName),
		//"application/json",
	}, ",")

	gv := v1.SchemeGroupVersion
	config.GroupVersion = &gv
	//config.APIPath = "/api"
	//config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	//config.UserAgent = rest.DefaultKubernetesUserAgent()

	client, err := rest.RESTClientFor(config)
	if err != nil {
		panic(err)
	}

	dynamicClient := dynamic.New(client)
	pods, err := dynamicClient.
		Resource(v1.SchemeGroupVersion.WithResource("pods")).
		Namespace("kube-system").
		List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err)
	}
	t.Logf("%v", len(pods.Items))
}
