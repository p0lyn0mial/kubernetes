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

package watch

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
)

func TestReadAheadDecoderCodecs(t *testing.T) {
	events := []watch.Event{
		{Type: watch.Added, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "one"}}},
		{Type: watch.Modified, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "two"}}},
		{Type: watch.Deleted, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "three"}}},
		{Type: watch.Error, Object: &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Message: "test"}},
		{Type: watch.Bookmark, Object: &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{ResourceVersion: "4"}}},
	}
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		t.Run(info.MediaType, func(t *testing.T) {
			var wire bytes.Buffer
			encoder := NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
			for i := range events {
				if err := encoder.Encode(&events[i]); err != nil {
					t.Fatal(err)
				}
			}
			stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(bytes.NewReader(wire.Bytes()))), info.StreamSerializer.Serializer)
			decoder := NewReadAheadDecoder(stream, scheme.Codecs.WithoutConversion().DecoderToVersion(info.Serializer, v1.SchemeGroupVersion))
			defer decoder.Close()
			for i, want := range events {
				action, object, err := decoder.Decode()
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				if action != want.Type {
					t.Fatalf("event %d: type %s, want %s", i, action, want.Type)
				}
				switch wantObject := want.Object.(type) {
				case *v1.Pod:
					pod, ok := object.(*v1.Pod)
					if !ok || pod.Name != wantObject.Name || pod.ResourceVersion != wantObject.ResourceVersion {
						t.Fatalf("event %d: object %#v, want %#v", i, object, wantObject)
					}
				case *metav1.Status:
					status, ok := object.(*metav1.Status)
					if !ok || status.Message != wantObject.Message {
						t.Fatalf("event %d: object %#v, want %#v", i, object, wantObject)
					}
				}
			}
			_, _, err := decoder.Decode()
			if !errors.Is(err, io.EOF) {
				t.Fatalf("terminal error = %v, want EOF", err)
			}
			_, _, err = decoder.Decode()
			if !errors.Is(err, io.EOF) {
				t.Fatalf("repeated terminal error = %v, want EOF", err)
			}
		})
	}
}

type observedStream struct {
	stream streaming.Decoder
	reads  chan struct{}
}

func (s *observedStream) Decode(defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	obj, gvk, err := s.stream.Decode(defaults, into)
	s.reads <- struct{}{}
	return obj, gvk, err
}

func (s *observedStream) Close() error { return s.stream.Close() }

type blockingObjectDecoder struct {
	runtime.Decoder
	started chan struct{}
	resume  chan struct{}
}

func (d *blockingObjectDecoder) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-d.resume
	return d.Decoder.Decode(data, defaults, into)
}

func TestReadAheadDecoderBufferedQueueAndClose(t *testing.T) {
	const payload = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"one"}}`
	var wire bytes.Buffer
	const events = defaultReadAheadBufferSize + 3
	for i := 0; i < events; i++ {
		wire.WriteString(`{"type":"ADDED","object":` + payload + "}\n")
	}
	info := scheme.Codecs.SupportedMediaTypes()[0]
	for _, candidate := range scheme.Codecs.SupportedMediaTypes() {
		if candidate.MediaType == runtime.ContentTypeJSON {
			info = candidate
			break
		}
	}
	stream := &observedStream{stream: streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(bytes.NewReader(wire.Bytes()))), info.StreamSerializer.Serializer), reads: make(chan struct{}, events)}
	object := &blockingObjectDecoder{Decoder: getDecoder(), started: make(chan struct{}, 1), resume: make(chan struct{})}
	decoder := NewReadAheadDecoder(stream, object)
	finished := make(chan error, 1)
	go func() {
		_, _, err := decoder.Decode()
		finished <- err
	}()
	select {
	case <-object.started:
	case <-time.After(time.Second):
		t.Fatal("object decode did not start")
	}
	for i := 0; i < defaultReadAheadBufferSize; i++ {
		select {
		case <-stream.reads:
		case <-time.After(time.Second):
			t.Fatal("envelope read did not progress")
		}
	}
	decoder.Close()
	close(object.resume)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active decode did not finish after Close")
	}
	if len(decoder.queue) != 0 {
		t.Fatalf("queued events retained after Close: %d", len(decoder.queue))
	}
}

func TestReadAheadDecoderPayloadOwnership(t *testing.T) {
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		t.Run(info.MediaType, func(t *testing.T) {
			var wire bytes.Buffer
			encoder := NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
			for _, name := range []string{"one", "two"} {
				if err := encoder.Encode(&watch.Event{Type: watch.Added, Object: &v1.Pod{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: name},
				}}); err != nil {
					t.Fatal(err)
				}
			}
			stream := &observedStream{
				stream: streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(bytes.NewReader(wire.Bytes()))), info.StreamSerializer.Serializer),
				reads:  make(chan struct{}, 3),
			}
			object := &blockingObjectDecoder{
				Decoder: scheme.Codecs.WithoutConversion().DecoderToVersion(info.Serializer, v1.SchemeGroupVersion),
				started: make(chan struct{}, 1), resume: make(chan struct{}),
			}
			decoder := NewReadAheadDecoder(stream, object)
			defer decoder.Close()
			result := make(chan runtime.Object, 1)
			go func() {
				_, obj, _ := decoder.Decode()
				result <- obj
			}()
			select {
			case <-object.started:
			case <-time.After(time.Second):
				t.Fatal("object decode did not start")
			}
			for i := 0; i < 2; i++ {
				select {
				case <-stream.reads:
				case <-time.After(time.Second):
					t.Fatal("second envelope was not read during first object decode")
				}
			}
			close(object.resume)
			select {
			case obj := <-result:
				pod, ok := obj.(*v1.Pod)
				if !ok || pod.Name != "one" {
					t.Fatalf("first object after second read = %#v", obj)
				}
			case <-time.After(time.Second):
				t.Fatal("first object decode stalled")
			}
			_, obj, err := decoder.Decode()
			pod, ok := obj.(*v1.Pod)
			if err != nil || !ok || pod.Name != "two" {
				t.Fatalf("second object = %#v, error = %v", obj, err)
			}
		})
	}
}

func TestReadAheadDecoderDefaultBuffer(t *testing.T) {
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON {
			continue
		}
		stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(strings.NewReader(""))), info.StreamSerializer.Serializer)
		decoder := NewReadAheadDecoder(stream, getDecoder())
		defer decoder.Close()
		if cap(decoder.queue) != defaultReadAheadBufferSize {
			t.Fatalf("queue capacity = %d, want %d", cap(decoder.queue), defaultReadAheadBufferSize)
		}
	}
}

type firstBlockedObjectDecoder struct {
	runtime.Decoder
	resume    chan struct{}
	completed chan string
}

func (d *firstBlockedObjectDecoder) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	obj, gvk, err := d.Decoder.Decode(data, defaults, into)
	if err != nil {
		return obj, gvk, err
	}
	name := obj.(*v1.Pod).Name
	if name == "0" {
		<-d.resume
	}
	d.completed <- name
	return obj, gvk, nil
}

func TestReadAheadDecoderProgressBehindSlowEvent(t *testing.T) {
	for _, concurrency := range []int{2, 4} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			const queueLength = 8
			const events = 20
			var wire bytes.Buffer
			for i := 0; i < events; i++ {
				wire.WriteString(`{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + strconv.Itoa(i) + `"}}}` + "\n")
			}
			var info runtime.SerializerInfo
			for _, candidate := range scheme.Codecs.SupportedMediaTypes() {
				if candidate.MediaType == runtime.ContentTypeJSON {
					info = candidate
					break
				}
			}
			stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(&wire)), info.StreamSerializer.Serializer)
			object := &firstBlockedObjectDecoder{
				Decoder: getDecoder(), resume: make(chan struct{}), completed: make(chan string, events),
			}
			decoder := NewReadAheadDecoderWithLoggerAndQueueLengthAndConcurrency(klog.Background(), stream, object, queueLength, concurrency)
			defer decoder.Close()
			// Release the blocked decoder even when an assertion fails.
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(object.resume) }) }
			defer release()
			for i := 0; i < queueLength+concurrency-1; i++ {
				select {
				case name := <-object.completed:
					if name == "0" {
						t.Fatal("first decode completed before release")
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d later events decoded while the first event was blocked", i)
				}
			}
			release()
			for i := 0; i < events; i++ {
				action, obj, err := decoder.Decode()
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				pod, ok := obj.(*v1.Pod)
				if action != watch.Added || !ok || pod.Name != strconv.Itoa(i) {
					t.Fatalf("event %d: type %s, object %#v", i, action, obj)
				}
			}
			if _, _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal error = %v, want EOF", err)
			}
		})
	}
}

func TestReadAheadDecoderQueueLength(t *testing.T) {
	for _, tc := range []struct {
		name        string
		queueLength int
		want        int
	}{
		{name: "configured", queueLength: 7, want: 7},
		{name: "default", queueLength: 0, want: defaultReadAheadBufferSize},
		{name: "negative uses default", queueLength: -1, want: defaultReadAheadBufferSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := scheme.Codecs.SupportedMediaTypes()[0]
			stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(strings.NewReader(""))), info.StreamSerializer.Serializer)
			decoder := NewReadAheadDecoderWithLoggerAndQueueLength(klog.Background(), stream, getDecoder(), tc.queueLength)
			defer decoder.Close()
			if got := cap(decoder.queue); got != tc.want {
				t.Fatalf("queue capacity = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadAheadDecoderObjectErrorWins(t *testing.T) {
	const wire = "{\"type\":\"ADDED\",\"object\":{\"metadata\":{\"name\":\"missing-kind\"}}}\n" +
		"{\"type\":\"INVALID\",\"object\":{}}\n"
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON {
			continue
		}
		stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(strings.NewReader(wire))), info.StreamSerializer.Serializer)
		decoder := NewReadAheadDecoder(stream, getDecoder())
		_, _, err := decoder.Decode()
		if err == nil || !strings.Contains(err.Error(), "unable to decode watch event") {
			t.Fatalf("error = %v, want object decoding error", err)
		}
		decoder.Close()
	}
}

func TestReadAheadDecoderCloseBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON {
			continue
		}
		stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(reader), info.StreamSerializer.Serializer)
		decoder := NewReadAheadDecoder(stream, getDecoder())
		finished := make(chan struct{})
		go func() {
			decoder.Close()
			decoder.Close()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("Close did not interrupt a blocked stream read")
		}
	}
}

func BenchmarkWatchDecoder(b *testing.B) {
	const events = 100
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON {
			continue
		}
		var wire bytes.Buffer
		encoder := NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
		for i := 0; i < events; i++ {
			if err := encoder.Encode(&watch.Event{Type: watch.Added, Object: &v1.Pod{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "benchmark"},
			}}); err != nil {
				b.Fatal(err)
			}
		}
		for _, mode := range []string{"serial", "read-ahead"} {
			b.Run(mode, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(bytes.NewReader(wire.Bytes()))), info.StreamSerializer.Serializer)
					var decoder watch.Decoder
					if mode == "serial" {
						decoder = NewDecoder(stream, getDecoder())
					} else {
						decoder = NewReadAheadDecoder(stream, getDecoder())
					}
					for j := 0; j < events; j++ {
						if _, _, err := decoder.Decode(); err != nil {
							b.Fatal(err)
						}
					}
					decoder.Close()
				}
			})
		}
	}
}
