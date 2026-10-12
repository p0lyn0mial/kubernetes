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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeprotobuf "k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	restclientwatch "k8s.io/client-go/rest/watch"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
)

func TestWatchListReadAheadSelection(t *testing.T) {
	config := &Config{Host: "http://localhost", GroupVersion: &v1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs, ExperimentalWatchListReadAhead: true}
	client, err := RESTClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Get().watchListReadAhead {
		t.Fatal("request did not inherit WatchList read-ahead setting")
	}
	if !CopyConfig(config).ExperimentalWatchListReadAhead {
		t.Fatal("copied config lost WatchList read-ahead setting")
	}
	for _, tc := range []struct {
		name      string
		initial   string
		mediaType string
		readAhead bool
	}{
		{"watchlist JSON", "true", runtime.ContentTypeJSON, true},
		{"watchlist protobuf", "true", runtime.ContentTypeProtobuf, true},
		{"ordinary watch", "", runtime.ContentTypeJSON, false},
		{"unsupported codec", "true", runtime.ContentTypeCBOR, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := client.Get()
			if tc.initial != "" {
				request.Param("sendInitialEvents", tc.initial)
			}
			stream := streaming.NewDecoder(io.NopCloser(strings.NewReader("")), scheme.Codecs.UniversalDeserializer())
			serializer := scheme.Codecs.SupportedMediaTypes()[0].StreamSerializer.Serializer
			for _, info := range scheme.Codecs.SupportedMediaTypes() {
				if info.MediaType == tc.mediaType {
					if info.StreamSerializer != nil {
						serializer = info.StreamSerializer.Serializer
					} else {
						serializer = info.Serializer
					}
					break
				}
			}
			if tc.mediaType == runtime.ContentTypeProtobuf {
				if _, ok := serializer.(*runtimeprotobuf.RawSerializer); !ok {
					t.Fatalf("protobuf stream serializer = %T, want *protobuf.RawSerializer", serializer)
				}
			}
			decoder := request.newWatchDecoder(context.Background(), tc.mediaType, serializer, stream, scheme.Codecs.UniversalDeserializer())
			defer decoder.Close()
			_, got := decoder.(*restclientwatch.ReadAheadDecoder)
			if got != tc.readAhead {
				t.Fatalf("read-ahead selected = %t, want %t", got, tc.readAhead)
			}
		})
	}
	config.ExperimentalWatchListReadAhead = false
	disabled, err := RESTClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	stream := streaming.NewDecoder(io.NopCloser(strings.NewReader("")), scheme.Codecs.UniversalDeserializer())
	decoder := disabled.Get().Param("sendInitialEvents", "true").newWatchDecoder(context.Background(), runtime.ContentTypeJSON, scheme.Codecs.SupportedMediaTypes()[0].Serializer, stream, scheme.Codecs.UniversalDeserializer())
	defer decoder.Close()
	if _, ok := decoder.(*restclientwatch.Decoder); !ok {
		t.Fatalf("disabled config selected %T, want serial decoder", decoder)
	}
}

func TestWatchListReadAheadFlow(t *testing.T) {
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		t.Run(info.MediaType, func(t *testing.T) {
			var wire bytes.Buffer
			encoder := restclientwatch.NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
			for _, event := range []watch.Event{
				{Type: watch.Added, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "one"}}},
				{Type: watch.Bookmark, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}}},
				{Type: watch.Modified, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "one", ResourceVersion: "3"}}},
			} {
				if err := encoder.Encode(&event); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("sendInitialEvents") != "true" {
					t.Errorf("sendInitialEvents = %q", r.URL.Query().Get("sendInitialEvents"))
				}
				w.Header().Set("Content-Type", info.MediaType)
				if _, err := w.Write(wire.Bytes()); err != nil {
					t.Errorf("write watch events: %v", err)
				}
			}))
			defer server.Close()
			client, err := RESTClientFor(&Config{
				Host: server.URL, APIPath: "/api", GroupVersion: &v1.SchemeGroupVersion,
				NegotiatedSerializer:           scheme.Codecs.WithoutConversion(),
				ExperimentalWatchListReadAhead: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stream, err := client.Get().Resource("pods").Param("watch", "true").Param("sendInitialEvents", "true").Watch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Stop()
			for i, want := range []watch.EventType{watch.Added, watch.Bookmark, watch.Modified} {
				select {
				case event, ok := <-stream.ResultChan():
					if !ok || event.Type != want {
						t.Fatalf("event %d = %#v (open %t), want %s", i, event, ok, want)
					}
					if _, ok := event.Object.(*v1.Pod); !ok {
						t.Fatalf("event %d object = %T, want *v1.Pod", i, event.Object)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}

func TestWatchListReadAheadEmitsPeriodicTraces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", runtime.ContentTypeJSON)
		if _, err := fmt.Fprintln(w, `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"one"}}}`); err != nil {
			t.Errorf("write watch event: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := RESTClientFor(&Config{
		Host: server.URL, APIPath: "/api", GroupVersion: &v1.SchemeGroupVersion,
		NegotiatedSerializer:           scheme.Codecs.WithoutConversion(),
		ExperimentalWatchListReadAhead: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := ktesting.NewLogger(t, ktesting.NewConfig(ktesting.Verbosity(0), ktesting.BufferLogs(true)))
	ctx, cancel := context.WithTimeout(klog.NewContext(context.Background(), logger), 20*time.Second)
	defer cancel()
	stream, err := client.Get().Resource("pods").Param("watch", "true").Param("sendInitialEvents", "true").Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	buffer := logger.GetSink().(ktesting.Underlier).GetBuffer()
	for {
		logs := buffer.String()
		if strings.Contains(logs, "WatchList read-ahead body trace") &&
			strings.Contains(logs, "WatchList streaming decoder trace") &&
			strings.Contains(logs, "WatchList read-ahead trace") &&
			strings.Contains(logs, "WatchList StreamWatcher waiting for result consumer") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("periodic traces missing before stream close: %s", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
