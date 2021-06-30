/*
Copyright 2021 The Kubernetes Authors.

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

package filters

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/util/rand"
)

// WithRetryAfter rejects any incoming new request(s) with a 429 if one of the provided conditions holds
//
// It includes new request(s) on a new or an existing TCP connection
// Any new request(s) arriving after a condition fulfills
// are replied with a 429 and the following response headers:
//   - 'Retry-After: N` (so client can retry after N seconds, hopefully on a new apiserver instance), where N is defined as [4, 12)
//   -  any optional headers set by a condition function
func WithRetryAfter(handler http.Handler, conditions ...func() ( /*shouldRun*/ bool /*rwMutator*/, func(w http.ResponseWriter) /*reason*/, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var ok bool
		var rwMutator func(w http.ResponseWriter)
		var reason string
		for _, conditionFn := range conditions {
			ok, rwMutator, reason = conditionFn()
			if ok {
				break
			}
		}

		if !ok {
			handler.ServeHTTP(w, req)
			return
		}

		if rwMutator != nil {
			rwMutator(w)
		}

		// Return a 429 status asking the cliet to try again after [4, 12) seconds
		retryAfter := rand.Intn(8) + 4
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		http.Error(w, reason, http.StatusTooManyRequests)
	})
}
