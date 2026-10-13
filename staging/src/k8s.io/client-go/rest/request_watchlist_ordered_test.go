/*
Copyright The Kubernetes Authors.

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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	restclientwatch "k8s.io/client-go/rest/watch"
)

func TestWatchListOrderedReadAheadSelection(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, initial := range []string{"", "false", "true"} {
			for _, info := range scheme.Codecs.SupportedMediaTypes() {
				if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
					continue
				}
				config := &Config{Host: "http://localhost", GroupVersion: &v1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs, ExperimentalWatchListReadAhead: enabled, ExperimentalWatchListReadAheadDecoder: WatchListReadAheadDecoderOrdered, ExperimentalWatchListReadAheadQueueLength: 7, ExperimentalWatchListReadAheadDecodeConcurrency: 2}
				client, err := RESTClientFor(config)
				if err != nil {
					t.Fatal(err)
				}
				stream := streaming.NewDecoder(io.NopCloser(strings.NewReader("")), info.StreamSerializer.Serializer)
				decoder := client.Get().Param("sendInitialEvents", initial).newWatchDecoder(context.Background(), info.MediaType, info.StreamSerializer.Serializer, stream, info.Serializer)
				decoder.Close()
				_, ordered := decoder.(*restclientwatch.OrderedReadAheadDecoder)
				if want := enabled && initial == "true"; ordered != want {
					t.Fatalf("enabled=%t initial=%q codec=%s: decoder %T, want ordered=%t", enabled, initial, info.MediaType, decoder, want)
				}
				if CopyConfig(config).ExperimentalWatchListReadAheadDecoder != WatchListReadAheadDecoderOrdered {
					t.Fatal("CopyConfig lost ordered selection")
				}
				if AnonymousClientConfig(config).ExperimentalWatchListReadAheadDecoder != WatchListReadAheadDecoderOrdered {
					t.Fatal("AnonymousClientConfig lost ordered selection")
				}
				request := client.Get()
				if request.watchListReadAheadQueueLength != 7 || request.watchListReadAheadDecodeConcurrency != 2 {
					t.Fatal("client lost read-ahead queue length or decode concurrency")
				}
			}
		}
	}
}

func TestWatchListOrderedReadAheadFlow(t *testing.T) {
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		t.Run(info.MediaType, func(t *testing.T) {
			const events = 20
			types := []watch.EventType{watch.Added, watch.Bookmark, watch.Modified, watch.Deleted}
			var wire bytes.Buffer
			encoder := restclientwatch.NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
			for i := 0; i < events; i++ {
				pod := &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: strconv.Itoa(i)}}
				if err := encoder.Encode(&watch.Event{Type: types[i%len(types)], Object: pod}); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("watch") != "true" || r.URL.Query().Get("sendInitialEvents") != "true" {
					t.Errorf("expected watch-list request, got query %q", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", info.MediaType)
				if _, err := w.Write(wire.Bytes()); err != nil {
					t.Errorf("write watch events: %v", err)
				}
			}))
			defer server.Close()
			client, err := RESTClientFor(&Config{Host: server.URL, APIPath: "/api", GroupVersion: &v1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs.WithoutConversion(), ExperimentalWatchListReadAhead: true, ExperimentalWatchListReadAheadDecoder: WatchListReadAheadDecoderOrdered, ExperimentalWatchListReadAheadQueueLength: 1, ExperimentalWatchListReadAheadDecodeConcurrency: 2})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.Get().Resource("pods").Param("watch", "true").Param("sendInitialEvents", "true").Watch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Stop()
			for i := 0; i < events; i++ {
				select {
				case event, ok := <-stream.ResultChan():
					pod, isPod := event.Object.(*v1.Pod)
					if !ok || event.Type != types[i%len(types)] || !isPod || pod.Name != strconv.Itoa(i) {
						t.Fatalf("event %d: %#v, open=%t", i, event, ok)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case event, ok := <-stream.ResultChan():
				if ok {
					t.Fatalf("expected EOF after %d events, got %#v", events, event)
				}
			case <-ctx.Done():
				t.Fatal("watch did not close after EOF")
			}
		})
	}
}
