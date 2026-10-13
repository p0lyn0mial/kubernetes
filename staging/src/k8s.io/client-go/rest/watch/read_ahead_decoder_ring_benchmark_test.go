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
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
)

type readAheadBenchmarkConfig struct{ queue, workers int }

var readAheadBenchmarkConfigs = []readAheadBenchmarkConfig{{100, 10}, {25000, 6}}

func benchmarkDecoder(mode string, stream streaming.Decoder, object runtime.Decoder, queue, workers int) watch.Decoder {
	switch mode {
	case "ring":
		return NewRingReadAheadDecoder(stream, object, queue, workers)
	case "concurrent":
		return NewReadAheadDecoderWithLoggerAndQueueLengthAndConcurrency(logr.Discard(), stream, object, queue, workers)
	case "serial":
		return NewDecoder(stream, object)
	default:
		panic("unknown benchmark decoder: " + mode)
	}
}

func closeBenchmarkDecoder(decoder watch.Decoder) {
	decoder.Close()
	if ring, ok := decoder.(*RingReadAheadDecoder); ok {
		<-ring.cleanupDone
	}
}

// Gate the measured events until warmup is fully consumed. Otherwise a large
// queue can predecode a significant part of b.N outside the timed region.
type gatedBenchmarkStream struct {
	streaming.Decoder
	warmup, remaining int
	begin, stopped    chan struct{}
	started           bool
	once              sync.Once
}

func (s *gatedBenchmarkStream) Decode(defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	if s.warmup > 0 {
		s.warmup--
	} else {
		if !s.started {
			select {
			case <-s.begin:
			case <-s.stopped:
				return nil, nil, io.EOF
			}
			s.started = true
		}
		if s.remaining == 0 {
			return nil, nil, io.EOF
		}
		s.remaining--
	}
	return s.Decoder.Decode(defaults, into)
}
func (s *gatedBenchmarkStream) Close() error {
	s.once.Do(func() { close(s.stopped) })
	return s.Decoder.Close()
}
func benchmarkStream(stream streaming.Decoder, warmup, events int) *gatedBenchmarkStream {
	return &gatedBenchmarkStream{Decoder: stream, warmup: warmup, remaining: events, begin: make(chan struct{}), stopped: make(chan struct{})}
}

type allocationFreeBenchmarkStream struct {
	remaining int
	payload   [1]byte
}

func (s *allocationFreeBenchmarkStream) Decode(_ *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	if s.remaining == 0 {
		return nil, nil, io.EOF
	}
	s.remaining--
	event := into.(*metav1.WatchEvent)
	event.Type = string(watch.Added)
	event.Object.Raw = s.payload[:]
	return event, nil, nil
}
func (*allocationFreeBenchmarkStream) Close() error { return nil }

type allocationFreeBenchmarkObject struct{ object v1.Pod }

func (d *allocationFreeBenchmarkObject) Decode(_ []byte, _ *schema.GroupVersionKind, _ runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	return &d.object, nil, nil
}

// BenchmarkReadAheadPipeline measures one event per operation. Immutable fake
// codecs allocate nothing, exposing allocations introduced by the pipeline.
// Construction and warmup are excluded; queue memory is measured separately.
func BenchmarkReadAheadPipeline(b *testing.B) {
	for _, config := range readAheadBenchmarkConfigs {
		for _, mode := range []string{"serial", "concurrent", "ring"} {
			b.Run(fmt.Sprintf("%s/queue=%d/workers=%d", mode, config.queue, config.workers), func(b *testing.B) {
				benchmarkReadAheadPipeline(b, mode, config.queue, config.workers)
			})
		}
	}
}

func benchmarkReadAheadPipeline(b *testing.B, mode string, queue, workers int) {
	warmup := 2*(queue+workers) + 128
	stream := benchmarkStream(&allocationFreeBenchmarkStream{remaining: b.N + warmup}, warmup, b.N)
	object := &allocationFreeBenchmarkObject{}
	decoder := benchmarkDecoder(mode, stream, object, queue, workers)
	defer closeBenchmarkDecoder(decoder)
	for i := 0; i < warmup; i++ {
		if _, _, err := decoder.Decode(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	close(stream.begin)
	for i := 0; i < b.N; i++ {
		action, obj, err := decoder.Decode()
		if err != nil || action != watch.Added || obj != &object.object {
			b.Fatalf("invalid event: %s, %T, %v", action, obj, err)
		}
	}
	b.StopTimer()
}

// cyclingBenchmarkReader repeats encoded frames without allocating a stream
// proportional to b.N. The transport is intentionally uncompressed so this
// benchmark isolates codec and pipeline costs from networking and gzip.
type cyclingBenchmarkReader struct {
	data   []byte
	offset int
	closed atomic.Bool
}

func (r *cyclingBenchmarkReader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, io.EOF
	}
	if r.offset == len(r.data) {
		r.offset = 0
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}
func (r *cyclingBenchmarkReader) Close() error { r.closed.Store(true); return nil }

// BenchmarkReadAheadCodecs includes fresh consumer-owned Pod allocation and
// real JSON/protobuf codecs. One operation is one event, after warmup.
func BenchmarkReadAheadCodecs(b *testing.B) {
	pod := &v1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "benchmark", Namespace: "default", ResourceVersion: "12345", Labels: map[string]string{"app": "benchmark", "component": "worker", "version": "v1"}},
		Spec:       v1.PodSpec{NodeName: "node-1", Containers: []v1.Container{{Name: "worker", Image: "example/worker:v1", Env: []v1.EnvVar{{Name: "MODE", Value: "benchmark"}, {Name: "REGION", Value: "local"}, {Name: "LOG_LEVEL", Value: "info"}}, Args: []string{"--serve", "--port=8080"}}, {Name: "sidecar", Image: "example/sidecar:v1"}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1", HostIP: "10.0.0.2"},
	}
	for _, info := range scheme.Codecs.SupportedMediaTypes() {
		if info.MediaType != runtime.ContentTypeJSON && info.MediaType != runtime.ContentTypeProtobuf {
			continue
		}
		var wire bytes.Buffer
		encoder := NewEncoder(streaming.NewEncoder(info.StreamSerializer.Framer.NewFrameWriter(&wire), info.StreamSerializer.Serializer), info.Serializer)
		if err := encoder.Encode(&watch.Event{Type: watch.Added, Object: pod}); err != nil {
			b.Fatal(err)
		}
		for _, config := range readAheadBenchmarkConfigs {
			for _, mode := range []string{"serial", "concurrent", "ring"} {
				b.Run(fmt.Sprintf("%s/%s/queue=%d/workers=%d", info.MediaType, mode, config.queue, config.workers), func(b *testing.B) {
					reader := &cyclingBenchmarkReader{data: wire.Bytes()}
					warmup := 2*(config.queue+config.workers) + 128
					stream := benchmarkStream(streaming.NewDecoder(info.StreamSerializer.Framer.NewFrameReader(reader), info.StreamSerializer.Serializer), warmup, b.N)
					object := scheme.Codecs.WithoutConversion().DecoderToVersion(info.Serializer, v1.SchemeGroupVersion)
					decoder := benchmarkDecoder(mode, stream, object, config.queue, config.workers)
					defer closeBenchmarkDecoder(decoder)
					for i := 0; i < warmup; i++ {
						if _, _, err := decoder.Decode(); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					b.SetBytes(int64(wire.Len()))
					b.ResetTimer()
					close(stream.begin)
					for i := 0; i < b.N; i++ {
						action, obj, err := decoder.Decode()
						if err != nil || action != watch.Added {
							b.Fatalf("invalid event: %s, %v", action, err)
						}
						decoded, ok := obj.(*v1.Pod)
						if !ok || decoded.Name != pod.Name {
							b.Fatalf("invalid object: %#v", obj)
						}
					}
					b.StopTimer()
				})
			}
		}
	}
}

// BenchmarkReadAheadConstruction keeps fixed startup costs and large-queue
// memory visible instead of hiding them in the steady-state measurements.
func BenchmarkReadAheadConstruction(b *testing.B) {
	for _, config := range readAheadBenchmarkConfigs {
		for _, mode := range []string{"concurrent", "ring"} {
			b.Run(fmt.Sprintf("%s/queue=%d/workers=%d", mode, config.queue, config.workers), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					stream := &allocationFreeBenchmarkStream{}
					decoder := benchmarkDecoder(mode, stream, &allocationFreeBenchmarkObject{}, config.queue, config.workers)
					closeBenchmarkDecoder(decoder)
				}
			})
		}
	}
}
