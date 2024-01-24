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
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"
)

// hasPathPrefix returns true if the string matches pathPrefix exactly, or if is prefixed with pathPrefix at a path segment boundary
func hasPathPrefix(s, pathPrefix string) bool {
	// Short circuit if s doesn't contain the prefix at all
	if !strings.HasPrefix(s, pathPrefix) {
		return false
	}

	pathPrefixLength := len(pathPrefix)

	if len(s) == pathPrefixLength {
		// Exact match
		return true
	}
	if strings.HasSuffix(pathPrefix, "/") {
		// pathPrefix already ensured a path segment boundary
		return true
	}
	if s[pathPrefixLength:pathPrefixLength+1] == "/" {
		// The next character in s is a path segment boundary
		// Check this instead of normalizing pathPrefix to avoid allocating on every call
		return true
	}
	return false
}

// storageResourceVersionGuard ensures that
// storage.GetCurrentResourceVersionFromStorage is computed
// only once.
//
// note that this struct is not thread safe.
type storageResourceVersionGuard struct {
	computedResourceVersion uint64
	computedErr             error

	underlyingStorage storage.Interface
	newListFunc       func() runtime.Object
	resourcePrefix    string
	objectType        string
}

// newStorageResourceVersionGuard creates a new instance of the storage guard.
// This function should be called for each new request.
func newStorageResourceVersionGuard(underlyingStorage storage.Interface, newListFunc func() runtime.Object, resourcePrefix, objectType string) *storageResourceVersionGuard {
	return &storageResourceVersionGuard{
		underlyingStorage: underlyingStorage,
		newListFunc:       newListFunc,
		resourcePrefix:    resourcePrefix,
		objectType:        objectType,
	}
}

// getCurrentResourceVersionFromStorageOnce calls storage.GetCurrentResourceVersionFromStorage once.
// the "once" semantic indicates that the previous call either returned an RV greater than 0 or encountered an error.
func (g *storageResourceVersionGuard) getCurrentResourceVersionFromStorageOnce(ctx context.Context) (uint64, error) {
	if g.computedResourceVersion > 0 || g.computedErr != nil {
		return g.computedResourceVersion, g.computedErr
	}
	g.computedResourceVersion, g.computedErr = storage.GetCurrentResourceVersionFromStorage(ctx, g.underlyingStorage, g.newListFunc, g.resourcePrefix, g.objectType)
	return g.computedResourceVersion, g.computedErr
}
