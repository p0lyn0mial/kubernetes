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
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestWithRetryAfter(t *testing.T) {
	tests := []struct {
		name               string
		notAcceptingFn     RetryConditionFn
		handlerInvoked     int
		closeExpected      string
		retryAfterExpected bool
		statusCodeExpected int
	}{
		{
			name:               "accepting new request",
			notAcceptingFn:     noopRetryCondition,
			handlerInvoked:     1,
			closeExpected:      "",
			retryAfterExpected: false,
			statusCodeExpected: http.StatusOK,
		},
		{
			name: "not accepting new request",
			notAcceptingFn: func() (bool, func(rw http.ResponseWriter), string) {
				return true, func(rw http.ResponseWriter) { rw.Header().Set("Connection", "close") }, ""
			},
			handlerInvoked:     0,
			closeExpected:      "close",
			retryAfterExpected: true,
			statusCodeExpected: http.StatusTooManyRequests,
		},
		{
			notAcceptingFn:     noopRetryCondition,
			name:               "empty condition function",
			handlerInvoked:     1,
			closeExpected:      "",
			retryAfterExpected: false,
			statusCodeExpected: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var handlerInvoked int
			handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				handlerInvoked++
			})

			wrapped := WithRetryAfter(handler, []RetryConditionFn{test.notAcceptingFn})

			request, err := http.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
			if err != nil {
				t.Fatalf("failed to create new http request - %v", err)
			}

			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, request)

			if test.handlerInvoked != handlerInvoked {
				t.Errorf("expected the handler to be invoked: %d timed, but got: %d", test.handlerInvoked, handlerInvoked)
			}
			if test.statusCodeExpected != w.Result().StatusCode {
				t.Errorf("expected status code: %d, but got: %d", test.statusCodeExpected, w.Result().StatusCode)
			}

			closeGot := w.Header().Get("Connection")
			if test.closeExpected != closeGot {
				t.Errorf("expected Connection close: %s, but got: %s", test.closeExpected, closeGot)
			}

			retryAfterGotStr := w.Header().Get("Retry-After")
			if len(retryAfterGotStr) > 0 && !test.retryAfterExpected {
				t.Error("didn't expect to find Retry-After Header")
			}

			if test.retryAfterExpected {
				retryAfterGot, err := strconv.Atoi(retryAfterGotStr)
				if err != nil {
					t.Error(err)
				}

				if !(retryAfterGot >= 4 && retryAfterGot < 12) {
					t.Errorf("expected Retry-After: [4, 12), but got: %d", retryAfterGot)
				}
			}
		})
	}
}

func noopRetryCondition() (bool, func(w http.ResponseWriter), string) {
	return false, nil, ""
}
