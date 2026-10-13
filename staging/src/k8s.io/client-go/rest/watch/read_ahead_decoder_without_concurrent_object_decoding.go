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
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

// SerialReadAheadDecoder reads envelopes ahead of the consumer, but decodes
// each object in Decode. This isolates envelope/network read-ahead from object
// decoding without adding worker scheduling or result reordering.
type SerialReadAheadDecoder struct {
	stream    streaming.Decoder
	object    runtime.Decoder
	queue     chan rawEvent
	done      chan struct{}
	readDone  chan struct{}
	traceDone chan struct{}
	once      sync.Once
	traceOnce sync.Once
	trace     *readAheadTrace
	terminal  error
}

func NewSerialReadAheadDecoder(stream streaming.Decoder, object runtime.Decoder) *SerialReadAheadDecoder {
	return NewSerialReadAheadDecoderWithLoggerAndQueueLength(klog.Background(), stream, object, 0)
}

func NewSerialReadAheadDecoderWithLoggerAndQueueLength(logger klog.Logger, stream streaming.Decoder, object runtime.Decoder, queueLength int) *SerialReadAheadDecoder {
	if queueLength <= 0 {
		queueLength = defaultReadAheadBufferSize
	}
	d := &SerialReadAheadDecoder{
		stream: stream, object: object, queue: make(chan rawEvent, queueLength),
		done: make(chan struct{}), readDone: make(chan struct{}), traceDone: make(chan struct{}),
		trace: &readAheadTrace{logger: logger},
	}
	go d.read()
	go d.reportTrace()
	return d
}

func (d *SerialReadAheadDecoder) read() {
	defer close(d.readDone)
	defer close(d.queue)
	for {
		select {
		case <-d.done:
			return
		default:
		}
		var got metav1.WatchEvent
		started := time.Now()
		d.trace.envelopeStart.Store(started.UnixNano())
		res, _, err := d.stream.Decode(nil, &got)
		d.trace.envelopes.Add(1)
		d.trace.envelopeNanos.Add(time.Since(started).Nanoseconds())
		d.trace.envelopeStart.Store(0)
		if err == nil && res != &got {
			err = fmt.Errorf("unable to decode to metav1.WatchEvent")
		}
		if err == nil {
			switch got.Type {
			case string(watch.Added), string(watch.Modified), string(watch.Deleted), string(watch.Error), string(watch.Bookmark):
			default:
				err = fmt.Errorf("got invalid watch event type: %v", got.Type)
			}
		}
		if err != nil {
			d.terminal = err
		}
		event := rawEvent{action: watch.EventType(got.Type), payload: got.Object.Raw}
		if err != nil {
			event = rawEvent{err: err}
		}
		started = time.Now()
		d.trace.queueWaitStart.Store(started.UnixNano())
		select {
		case d.queue <- event:
			d.trace.queueWaitNanos.Add(time.Since(started).Nanoseconds())
			d.trace.queueWaitStart.Store(0)
			d.trace.observeQueueDepth(len(d.queue))
		case <-d.done:
			d.trace.queueWaitStart.Store(0)
			return
		}
		if err != nil {
			return
		}
	}
}

func (d *SerialReadAheadDecoder) decodeEvent(event rawEvent) decodeResult {
	if event.err != nil {
		return decodeResult{err: event.err}
	}
	started := time.Now()
	d.trace.objectStart.Store(started.UnixNano())
	obj, err := runtime.Decode(d.object, event.payload)
	d.trace.objects.Add(1)
	d.trace.objectNanos.Add(time.Since(started).Nanoseconds())
	d.trace.objectStart.Store(0)
	if err != nil {
		return decodeResult{err: fmt.Errorf("unable to decode watch event: %v", err), closeOnError: true}
	}
	return decodeResult{action: event.action, object: obj}
}

func (d *SerialReadAheadDecoder) Decode() (watch.EventType, runtime.Object, error) {
	started := time.Now()
	d.trace.consumerStart.Store(started.UnixNano())
	select {
	case event, ok := <-d.queue:
		d.trace.consumerWaitNanos.Add(time.Since(started).Nanoseconds())
		d.trace.consumerStart.Store(0)
		if !ok {
			if d.terminal != nil {
				return "", nil, d.terminal
			}
			return "", nil, errReadAheadExhausted
		}
		result := d.decodeEvent(event)
		if result.err != nil {
			if result.closeOnError {
				d.Close()
			}
			return "", nil, result.err
		}
		return result.action, result.object, nil
	case <-d.done:
		d.trace.consumerStart.Store(0)
		return "", nil, errReadAheadClosed
	}
}

func (d *SerialReadAheadDecoder) StreamWatcherTraceEnabled() bool { return true }

func (d *SerialReadAheadDecoder) reportTrace() {
	defer close(d.traceDone)
	ticker := time.NewTicker(readAheadTraceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.reportSnapshot("periodic")
		case <-d.done:
			return
		}
	}
}

func (d *SerialReadAheadDecoder) reportSnapshot(phase string) {
	if reporter, ok := d.stream.(interface{ ReportTrace(string) }); ok {
		reporter.ReportTrace(phase)
	}
	d.trace.log(phase, len(d.queue), cap(d.queue))
}

func (d *SerialReadAheadDecoder) Close() {
	d.once.Do(func() {
		close(d.done)
		d.stream.Close()
	})
	<-d.readDone
	<-d.traceDone
	d.traceOnce.Do(func() { d.reportSnapshot("final") })
}

var _ watch.Decoder = (*SerialReadAheadDecoder)(nil)
