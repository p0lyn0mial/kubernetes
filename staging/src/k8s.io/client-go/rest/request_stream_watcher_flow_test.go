/*
Copyright 2026 The Kubernetes Authors.

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

package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
)

// TestNewStreamWatcherFlow keeps the wire event literal so its bytes can be compared
// with the object delivered by ResultChan while stepping through the decoder.
// Set breakpoints in Request.Watch, Request.newStreamWatcher, and StreamWatcher.receive.
func TestNewStreamWatcherFlow(t *testing.T) {
	const wireEvent = `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"example","resourceVersion":"7"}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" || r.URL.Query().Get("watch") != "true" {
			t.Errorf("request = %s, want /api/v1/pods?watch=true", r.URL)
		}
		t.Logf("server: %s %s", r.Method, r.URL)
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintln(w, wireEvent); err != nil {
			t.Errorf("write watch event: %v", err)
		}
	}))
	defer server.Close()

	client, err := RESTClientFor(&Config{
		Host:                 server.URL,
		APIPath:              "/api",
		GroupVersion:         &corev1.SchemeGroupVersion,
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Hour)
	defer cancel()

	t.Log("client: Watch calls newStreamWatcher after the HTTP response")
	stream, err := client.Get().Resource("pods").Param("watch", "true").Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()

	select {
	case event, ok := <-stream.ResultChan():
		if !ok {
			t.Fatal("watch closed before delivering the event")
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			t.Fatalf("decoded object = %T, want *v1.Pod", event.Object)
		}
		t.Logf("client: ResultChan delivered %s Pod %q (resourceVersion %q)", event.Type, pod.Name, pod.ResourceVersion)
		if event.Type != watch.Added || pod.Name != "example" || pod.ResourceVersion != "7" {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for watch event: %v", ctx.Err())
	}
}
