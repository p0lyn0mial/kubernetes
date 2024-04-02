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

package validation

import (
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/apis/core"
)

var (
	// we make an exception for the following secret objects, during update
	// we allow the secret type to mutate from:
	//     "SecretTypeTLS" -> "kubernetes.io/tls"
	// some of our operators were accidentally creating secrets of type
	// "SecretTypeTLS", and this patch enables us to move these secrest
	// objects to the intended type in a ratcheting manner.
	//
	// we can drop this patch when we migrate all of the affected secret
	// objects to to intended type: https://issues.redhat.com/browse/API-1800
	whitelist = map[apimachinerytypes.NamespacedName]struct{}{
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
)

func OpenShiftValidateSecretUpdateIsTypeMutationAllowed(newSecret, oldSecret *core.Secret) bool {
	// we allow "SecretTypeTLS" -> "kubernetes.io/tls" only
	if oldSecret.Type == "SecretTypeTLS" && newSecret.Type == core.SecretTypeTLS {
		key := apimachinerytypes.NamespacedName{Namespace: oldSecret.Namespace, Name: oldSecret.Name}
		if _, ok := whitelist[key]; ok {
			return true
		}
	}
	return false
}
