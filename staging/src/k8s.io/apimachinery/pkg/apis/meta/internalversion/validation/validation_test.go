/*
Copyright 2020 The Kubernetes Authors.

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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateListOptions(t *testing.T) {
	cases := []struct {
		name                    string
		opts                    internalversion.ListOptions
		watchListFeatureEnabled bool
		expectError             string
	}{
		{
			name: "valid-default",
			opts: internalversion.ListOptions{},
		},
		{
			name: "valid-resourceversionmatch-exact",
			opts: internalversion.ListOptions{
				ResourceVersion:      "1",
				ResourceVersionMatch: metav1.ResourceVersionMatchExact,
			},
		},
		{
			name: "invalid-resourceversionmatch-exact",
			opts: internalversion.ListOptions{
				ResourceVersion:      "0",
				ResourceVersionMatch: metav1.ResourceVersionMatchExact,
			},
			expectError: "resourceVersionMatch: Forbidden: resourceVersionMatch \"exact\" is forbidden for resourceVersion \"0\"",
		},
		{
			name: "valid-resourceversionmatch-notolderthan",
			opts: internalversion.ListOptions{
				ResourceVersion:      "0",
				ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
			},
		},
		{
			name: "invalid-resourceversionmatch",
			opts: internalversion.ListOptions{
				ResourceVersion:      "0",
				ResourceVersionMatch: "foo",
			},
			expectError: "resourceVersionMatch: Unsupported value: \"foo\": supported values: \"Exact\", \"NotOlderThan\", \"\"",
		},
		{
			name: "watch-resourceversionmatch-forbidden",
			opts: internalversion.ListOptions{
				Watch:                true,
				ResourceVersionMatch: "foo",
			},
			expectError: "resourceVersionMatch: Forbidden: resourceVersionMatch is forbidden for watch",
		},
		{
			name: "watch-sendInitialEvents-forbidden",
			opts: internalversion.ListOptions{
				Watch:               true,
				SendInitialEvents:   true,
				AllowWatchBookmarks: true,
			},
			expectError: "sendInitialEvents: Forbidden: sendInitialEvents is forbidden for watch",
		},
		{
			name: "watch-sendInitialEvents-forbidden-without-bookmarks",
			opts: internalversion.ListOptions{
				Watch:             true,
				SendInitialEvents: true,
			},
			watchListFeatureEnabled: true,
			expectError:             `sendInitialEvents: Forbidden: sendInitialEvents is forbidden when allowWatchBookmarks is disabled`,
		},
		{
			name: "watch-sendInitialEvents",
			opts: internalversion.ListOptions{
				Watch:               true,
				SendInitialEvents:   true,
				AllowWatchBookmarks: true,
			},
			watchListFeatureEnabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateListOptions(&tc.opts, tc.watchListFeatureEnabled)
			if tc.expectError != "" {
				if len(errs) != 1 {
					t.Errorf("expected an error but got %d errors", len(errs))
				} else if errs[0].Error() != tc.expectError {
					t.Errorf("expected error '%s' but got '%s'", tc.expectError, errs[0].Error())
				}
				return
			}
			if len(errs) != 0 {
				t.Errorf("expected no errors, but got: %v", errs)
			}
		})
	}
}
