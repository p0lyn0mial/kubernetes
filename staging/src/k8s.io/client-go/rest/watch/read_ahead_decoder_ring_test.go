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

package watch

import (
	"bytes"
	"errors"
	"fmt"
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
)

func TestRingReadAheadDecoderCodecsAndWraparound(t *testing.T) {
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		t.Run(info.MediaType, func(t *testing.T) {
			var wire bytes.Buffer
			encoder := NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
			const events = 100
			types := []watch.EventType{watch.Added, watch.Modified, watch.Deleted, watch.Bookmark, watch.Error}
			expected := make([]runtime.Object, events)
			for i := range expected {
				if types[i%len(types)] == watch.Error {
					expected[i] = &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Message: strconv.Itoa(i)}
				} else {
					expected[i] = &v1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: strconv.Itoa(i)}}
				}
				if err := encoder.Encode(&watch.Event{Type: types[i%len(types)], Object: expected[i]}); err != nil {
					t.Fatal(err)
				}
			}
			stream := streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(io.NopCloser(&wire)), info.StreamSerializer.Serializer)
			decoder := NewRingReadAheadDecoder(stream, scheme.Codecs.WithoutConversion().DecoderToVersion(info.Serializer, v1.SchemeGroupVersion), 1, 2)
			defer closeRingTestDecoder(t, decoder)
			// Retain every returned object across repeated slot reuse.
			retained := make([]runtime.Object, events)
			for i := range retained {
				action, obj, err := decoder.Decode()
				if err != nil {
					t.Fatal(err)
				}
				if action != types[i%len(types)] {
					t.Fatalf("event %d: type %s", i, action)
				}
				retained[i] = obj
			}
			for i, obj := range retained {
				switch obj := obj.(type) {
				case *v1.Pod:
					if obj.Name != strconv.Itoa(i) {
						t.Fatalf("event %d overwritten: %#v", i, obj)
					}
				case *metav1.Status:
					if obj.Message != strconv.Itoa(i) {
						t.Fatalf("event %d overwritten: %#v", i, obj)
					}
				default:
					t.Fatalf("event %d: unexpected object %T", i, obj)
				}
			}
			for i := 0; i < 2; i++ {
				if _, _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
					t.Fatalf("terminal error = %v, want EOF", err)
				}
			}
		})
	}
}

// These immutable payloads and objects isolate pipeline behavior from codecs.
type ringTestStream struct {
	payloads [][]byte
	next     int
	action   string
	terminal error
}

func (s *ringTestStream) Decode(_ *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	if s.next == len(s.payloads) {
		if s.terminal != nil {
			return nil, nil, s.terminal
		}
		return nil, nil, io.EOF
	}
	event := into.(*metav1.WatchEvent)
	event.Type = s.action
	event.Object.Raw = s.payloads[s.next]
	s.next++
	return event, nil, nil
}
func (*ringTestStream) Close() error { return nil }

type ringTestObjectDecoder struct {
	objects   []v1.Pod
	resume    chan struct{}
	completed chan int
	fail      int
}

func (d *ringTestObjectDecoder) Decode(data []byte, _ *schema.GroupVersionKind, _ runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	n := int(data[0])
	if n == 0 && d.resume != nil {
		<-d.resume
	}
	if d.completed != nil {
		d.completed <- n
	}
	if n == d.fail {
		return nil, nil, errors.New("test object failure")
	}
	return &d.objects[n], nil, nil
}

func TestRingReadAheadDecoderOrderedResultsAndErrors(t *testing.T) {
	for _, fail := range []int{-1, 2} {
		for _, concurrency := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("fail=%d/workers=%d", fail, concurrency), func(t *testing.T) {
				const events = 20
				const queue = 3
				stream := &ringTestStream{action: string(watch.Added)}
				object := &ringTestObjectDecoder{objects: make([]v1.Pod, events), resume: make(chan struct{}), completed: make(chan int, events), fail: fail}
				for i := 0; i < events; i++ {
					stream.payloads = append(stream.payloads, []byte{byte(i)})
					object.objects[i].Name = strconv.Itoa(i)
				}
				decoder := NewRingReadAheadDecoder(stream, object, queue, concurrency)
				defer closeRingTestDecoder(t, decoder)
				var once sync.Once
				release := func() { once.Do(func() { close(object.resume) }) }
				defer release()
				if concurrency > 1 {
					// Every other slot can finish while the first slot is blocked.
					for i := 0; i < queue+concurrency-1; i++ {
						select {
						case n := <-object.completed:
							if n == 0 {
								t.Fatal("first event completed before release")
							}
						case <-time.After(5 * time.Second):
							t.Fatalf("later decodes stalled after %d events", i)
						}
					}
				}
				release()
				for i := 0; i < events; i++ {
					action, obj, err := decoder.Decode()
					if i == fail {
						if err == nil || !strings.Contains(err.Error(), "unable to decode watch event: test object failure") {
							t.Fatalf("event %d: error %v", i, err)
						}
						if _, _, err = decoder.Decode(); !errors.Is(err, errReadAheadClosed) {
							t.Fatalf("after object error: %v", err)
						}
						return
					}
					if err != nil || action != watch.Added || obj != &object.objects[i] {
						t.Fatalf("event %d: %s, %#v, %v", i, action, obj, err)
					}
				}
				if _, _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
					t.Fatalf("terminal error: %v", err)
				}
			})
		}
	}
}

func TestRingReadAheadDecoderEnvelopeErrors(t *testing.T) {
	for _, action := range []string{"ADDED", "INVALID"} {
		t.Run(action, func(t *testing.T) {
			terminal := errors.New("test stream failure")
			stream := &ringTestStream{action: action, payloads: [][]byte{{0}}, terminal: terminal}
			object := &ringTestObjectDecoder{objects: make([]v1.Pod, 1), fail: -1}
			decoder := NewRingReadAheadDecoder(stream, object, 1, 1)
			defer closeRingTestDecoder(t, decoder)
			_, _, err := decoder.Decode()
			if action == "ADDED" {
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = decoder.Decode()
			}
			if action == "ADDED" && !errors.Is(err, terminal) {
				t.Fatalf("error=%v, want stream failure", err)
			}
			if action == "INVALID" && (err == nil || !strings.Contains(err.Error(), "invalid watch event type")) {
				t.Fatalf("error=%v, want invalid type", err)
			}
			_, _, again := decoder.Decode()
			if again != err {
				t.Fatalf("terminal error changed: %v -> %v", err, again)
			}
		})
	}
}

func TestRingReadAheadDecoderClose(t *testing.T) {
	t.Run("blocked stream", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		decoder := NewRingReadAheadDecoder(streaming.NewDecoder(reader, getDecoder()), getDecoder(), 1, 1)
		decoder.Close()
		decoder.Close()
		select {
		case <-decoder.cleanupDone:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup stalled")
		}
		if _, _, err := decoder.Decode(); !errors.Is(err, errReadAheadClosed) {
			t.Fatalf("after Close: %v", err)
		}
	})
	t.Run("blocked object", func(t *testing.T) {
		object := &blockingObjectDecoder{Decoder: getDecoder(), started: make(chan struct{}, 1), resume: make(chan struct{})}
		stream := &ringTestStream{action: "ADDED", payloads: [][]byte{[]byte(`{"apiVersion":"v1","kind":"Pod"}`)}}
		decoder := NewRingReadAheadDecoder(stream, object, 1, 1)
		var once sync.Once
		defer once.Do(func() { close(object.resume) })
		defer closeRingTestDecoder(t, decoder)
		finished := make(chan error, 1)
		go func() { _, _, err := decoder.Decode(); finished <- err }()
		select {
		case <-object.started:
		case <-time.After(5 * time.Second):
			t.Fatal("decode did not start")
		}
		decoder.Close()
		select {
		case err := <-finished:
			if !errors.Is(err, errReadAheadClosed) {
				t.Fatalf("Decode: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Decode did not stop")
		}
		once.Do(func() { close(object.resume) })
		select {
		case <-decoder.cleanupDone:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not finish")
		}
		for i := range decoder.slots {
			if decoder.slots[i].envelope.Object.Raw != nil || decoder.slots[i].result.object != nil || len(decoder.slots[i].ready) != 0 {
				t.Fatalf("slot %d retained payload or object", i)
			}
		}
	})
}

func TestRingReadAheadDecoderPipelineAllocations(t *testing.T) {
	stream := &allocationFreeBenchmarkStream{remaining: 1 << 30}
	decoder := NewRingReadAheadDecoder(stream, &allocationFreeBenchmarkObject{}, 100, 6)
	defer closeRingTestDecoder(t, decoder)
	for i := 0; i < 256; i++ {
		if _, _, err := decoder.Decode(); err != nil {
			t.Fatal(err)
		}
	}
	// AllocsPerRun warms up and fixes GOMAXPROCS to avoid benchmark framework
	// allocations being attributed to asynchronous pipeline work.
	allocs := testing.AllocsPerRun(10000, func() {
		if _, _, err := decoder.Decode(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("pipeline allocations per event = %g, want 0", allocs)
	}
}

func closeRingTestDecoder(t *testing.T, d *RingReadAheadDecoder) {
	t.Helper()
	d.Close()
	select {
	case <-d.cleanupDone:
	case <-time.After(5 * time.Second):
		t.Error("decoder cleanup did not finish")
	}
}

func TestRingReadAheadDecoderConcurrentCloseAndConsumption(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		stream := &allocationFreeBenchmarkStream{remaining: 1000}
		decoder := NewRingReadAheadDecoder(stream, &allocationFreeBenchmarkObject{}, 1, 4)
		consumed := make(chan error, 1)
		go func() {
			for {
				if _, _, err := decoder.Decode(); err != nil {
					consumed <- err
					return
				}
			}
		}()
		var closers sync.WaitGroup
		closers.Add(2)
		for i := 0; i < 2; i++ {
			go func() { defer closers.Done(); decoder.Close() }()
		}
		closers.Wait()
		select {
		case err := <-consumed:
			if !errors.Is(err, errReadAheadClosed) && !errors.Is(err, io.EOF) {
				t.Fatalf("iteration %d: %v", iteration, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: consumption did not stop", iteration)
		}
		closeRingTestDecoder(t, decoder)
		for i := range decoder.slots {
			if decoder.slots[i].result.object != nil || decoder.slots[i].envelope.Object.Raw != nil {
				t.Fatalf("iteration %d slot %d retained an object", iteration, i)
			}
		}
	}
}
