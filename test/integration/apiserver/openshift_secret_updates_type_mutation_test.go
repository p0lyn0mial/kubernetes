/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/integration/framework"
)

// the list was copied from pkg/apis/core/validation/validation_patch.go
var whitelistedSecrets = map[apimachinerytypes.NamespacedName]struct{}{
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "aggregator-client-signer"}:          {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "kube-apiserver-to-kubelet-signer"}:  {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "localhost-serving-signer"}:          {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "service-network-serving-signer"}:    {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "loadbalancer-serving-signer"}:       {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "localhost-recovery-serving-signer"}: {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "kube-control-plane-signer"}:         {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-apiserver-operator", Name: "node-system-admin-signer"}:          {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-etcd-operator", Name: "etcd-client"}:                                 {},
	apimachinerytypes.NamespacedName{Namespace: "openshift-kube-controller-manager-operator", Name: "csr-signer-signer"}:        {},
}

func TestOpenShiftValidateWhiteListedSecretTypeMutationUpdateAllowed(t *testing.T) {
	ctx := context.Background()
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	e2eframework.ExpectNoError(err)
	t.Cleanup(server.TearDownFn)
	client, err := kubernetes.NewForConfig(server.ClientConfig)
	e2eframework.ExpectNoError(err)

	for whiteListedSecret := range whitelistedSecrets {
		_, err := client.CoreV1().Namespaces().Get(ctx, whiteListedSecret.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			testNamespace := framework.CreateNamespaceOrDie(client, whiteListedSecret.Namespace, t)
			t.Cleanup(func() { framework.DeleteNamespaceOrDie(client, testNamespace, t) })
		} else if err != nil {
			t.Fatal(err)
		}

		secret := constructSecretWithOldType(whiteListedSecret.Namespace, whiteListedSecret.Name)
		createdSecret, err := client.CoreV1().Secrets(whiteListedSecret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
		e2eframework.ExpectNoError(err, "failed to create secret")

		createdSecret.Type = corev1.SecretTypeTLS
		updatedSecret, err := client.CoreV1().Secrets(whiteListedSecret.Namespace).Update(ctx, createdSecret, metav1.UpdateOptions{})
		e2eframework.ExpectNoError(err, "failed to update the type of the secret")
		if updatedSecret.Type != corev1.SecretTypeTLS {
			t.Errorf("unexpected type of the secret = %v, expected = %v", updatedSecret.Type, corev1.SecretTypeTLS)
		}
	}
}

func TestNotWhiteListedSecretTypeMutationUpdateDisallowed(t *testing.T) {
	ctx := context.Background()
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	e2eframework.ExpectNoError(err)
	t.Cleanup(server.TearDownFn)
	client, err := kubernetes.NewForConfig(server.ClientConfig)
	e2eframework.ExpectNoError(err)

	testNamespace := framework.CreateNamespaceOrDie(client, "secret-type-update-disallowed", t)
	t.Cleanup(func() { framework.DeleteNamespaceOrDie(client, testNamespace, t) })

	secret := constructSecretWithOldType(testNamespace.Name, "foo")
	createdSecret, err := client.CoreV1().Secrets(testNamespace.Name).Create(ctx, secret, metav1.CreateOptions{})
	e2eframework.ExpectNoError(err, "failed to create secret")

	createdSecret.Type = corev1.SecretTypeTLS
	_, err = client.CoreV1().Secrets(testNamespace.Name).Update(ctx, createdSecret, metav1.UpdateOptions{})
	if !apierrors.IsInvalid(err) {
		t.Errorf("unexpected error returned: %v", err)
	}
}

func constructSecretWithOldType(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Type: "SecretTypeTLS",
		Data: map[string][]byte{"tls.crt": {}, "tls.key": {}},
	}
}
