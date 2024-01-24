/*
Copyright 2015 The Kubernetes Authors.

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

package cacher

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/storage"
)

func TestHasPathPrefix(t *testing.T) {
	validTestcases := []struct {
		s      string
		prefix string
	}{
		// Exact matches
		{"", ""},
		{"a", "a"},
		{"a/", "a/"},
		{"a/../", "a/../"},

		// Path prefix matches
		{"a/b", "a"},
		{"a/b", "a/"},
		{"中文/", "中文"},
	}
	for i, tc := range validTestcases {
		if !hasPathPrefix(tc.s, tc.prefix) {
			t.Errorf(`%d: Expected hasPathPrefix("%s","%s") to be true`, i, tc.s, tc.prefix)
		}
	}

	invalidTestcases := []struct {
		s      string
		prefix string
	}{
		// Mismatch
		{"a", "b"},

		// Dir requirement
		{"a", "a/"},

		// Prefix mismatch
		{"ns2", "ns"},
		{"ns2", "ns/"},
		{"中文文", "中文"},

		// Ensure no normalization is applied
		{"a/c/../b/", "a/b/"},
		{"a/", "a/b/.."},
	}
	for i, tc := range invalidTestcases {
		if hasPathPrefix(tc.s, tc.prefix) {
			t.Errorf(`%d: Expected hasPathPrefix("%s","%s") to be false`, i, tc.s, tc.prefix)
		}
	}
}

func TestStorageResourceVersionGuard(t *testing.T) {
	ctx, store, terminate := testSetup(t)
	t.Cleanup(terminate)

	makePod := func(name string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		}
	}
	createPod := func(ctx context.Context, obj *example.Pod) *example.Pod {
		key := "pods/" + obj.Namespace + "/" + obj.Name
		out := &example.Pod{}
		err := store.Create(ctx, key, obj, out, 0)
		require.NoError(t, err)
		return out
	}
	versioner := storage.APIObjectVersioner{}

	t.Run("happy path: the target calls GetCurrentResourceVersionFromStorage only once", func(t *testing.T) {
		target := newStorageResourceVersionGuard(store, func() runtime.Object { return &example.PodList{} }, "/pods", "Pod")

		t.Logf("Creating a pod, getting the global RV from the store and comparing the RV with the pod")
		pod := createPod(ctx, makePod("pod-1"))
		currentStorageRV, err := storage.GetCurrentResourceVersionFromStorage(context.TODO(), store, func() runtime.Object { return &example.PodList{} }, "/pods", "Pod")
		require.NoError(t, err)
		podRV, err := versioner.ParseResourceVersion(pod.ResourceVersion)
		require.NoError(t, err)
		require.Equal(t, currentStorageRV, podRV, "expected the global etcd RV to be equal to pod's RV")

		t.Log("Getting the current RV from the storage version guard (target) and comparing with the one retrieved directly from the store")
		targetRV, err := target.getCurrentResourceVersionFromStorageOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, targetRV, currentStorageRV, "expected the target RV to be equal to the global etcd RV")

		t.Log("Advancing the global RV by creating a new pod and checking if it is different from target's RV")
		_ = createPod(ctx, makePod("pod-2"))
		previousStorageRV := currentStorageRV
		currentStorageRV, err = storage.GetCurrentResourceVersionFromStorage(context.TODO(), store, func() runtime.Object { return &example.PodList{} }, "/pods", "Pod")
		require.NoError(t, err)
		require.Greater(t, currentStorageRV, previousStorageRV, "expected the global RV to be greater that the previously retrieved RV")
		targetRV, err = target.getCurrentResourceVersionFromStorageOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, targetRV, previousStorageRV, "expected the target RV to be equal to the previously retrieved RV")
	})

	t.Run("even on err the target calls GetCurrentResourceVersionFromStorage only once", func(t *testing.T) {
		expectedError := errors.New("dummy error")
		wrappedStorage := &wrappedStorageForListError{store, expectedError}
		target := newStorageResourceVersionGuard(wrappedStorage, func() runtime.Object { return &example.PodList{} }, "/pods", "Pod")

		t.Log("Getting the current RV from the storage version guard (target) and comparing with the expected error")
		targetRV, err := target.getCurrentResourceVersionFromStorageOnce(ctx)
		require.Equal(t, expectedError, err)
		require.Equal(t, targetRV, uint64(0), "expected the targetRV to be 0")

		t.Log("Updating the wrapped storage error and checking if the target returns the previous one")
		wrappedStorage.err = errors.New("new dummy error")
		targetRV, err = target.getCurrentResourceVersionFromStorageOnce(ctx)
		require.Equal(t, expectedError, err)
		require.Equal(t, targetRV, uint64(0), "expected the targetRV to be 0")
	})
}

type wrappedStorageForListError struct {
	storage.Interface
	err error
}

func (w *wrappedStorageForListError) GetList(_ context.Context, _ string, _ storage.ListOptions, _ runtime.Object) error {
	return w.err
}
