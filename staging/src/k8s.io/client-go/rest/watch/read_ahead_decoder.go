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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

var (
	errReadAheadClosed    = errors.New("watch decoder closed")
	errReadAheadExhausted = errors.New("watch decoder exhausted")
)

const defaultReadAheadBufferSize = 100
const readAheadTraceInterval = 5 * time.Second
const readAheadDecodeConcurrency = 10

type readAheadTrace struct {
	logger            klog.Logger
	envelopes         atomic.Int64
	envelopeNanos     atomic.Int64
	queueWaitNanos    atomic.Int64
	objects           atomic.Int64
	objectNanos       atomic.Int64
	workerQueueNanos  atomic.Int64
	resultWaitNanos   atomic.Int64
	consumerWaitNanos atomic.Int64
	peakQueue         atomic.Int64
	envelopeStart     atomic.Int64
	queueWaitStart    atomic.Int64
	objectStart       atomic.Int64
	objectActive      atomic.Int64
	workerActive      atomic.Int64
	peakWorkers       atomic.Int64
	workerQueueStart  atomic.Int64
	resultWaitStart   atomic.Int64
	consumerStart     atomic.Int64
}

func activeDuration(start int64) time.Duration {
	if start == 0 {
		return 0
	}
	return time.Since(time.Unix(0, start))
}

func (t *readAheadTrace) log(phase string, queueDepth, queueCapacity int) {
	t.logger.Info("WatchList read-ahead trace",
		"phase", phase,
		"envelopes", t.envelopes.Load(),
		"envelopeReadDecodeTime", time.Duration(t.envelopeNanos.Load()),
		"envelopeReadDecodeInProgress", activeDuration(t.envelopeStart.Load()),
		"queueWaitTime", time.Duration(t.queueWaitNanos.Load()),
		"queueWaitInProgress", activeDuration(t.queueWaitStart.Load()),
		"objects", t.objects.Load(),
		"objectDecodeTime", time.Duration(t.objectNanos.Load()),
		"objectDecodeInProgress", activeDuration(t.objectStart.Load()),
		"activeWorkers", t.workerActive.Load(),
		"peakWorkers", t.peakWorkers.Load(),
		"workerQueueWaitTime", time.Duration(t.workerQueueNanos.Load()),
		"workerQueueWaitInProgress", activeDuration(t.workerQueueStart.Load()),
		"orderedResultWaitTime", time.Duration(t.resultWaitNanos.Load()),
		"orderedResultWaitInProgress", activeDuration(t.resultWaitStart.Load()),
		"consumerWaitTime", time.Duration(t.consumerWaitNanos.Load()),
		"consumerWaitInProgress", activeDuration(t.consumerStart.Load()),
		"peakQueueDepth", t.peakQueue.Load(),
		"queueDepth", queueDepth,
		"queueCapacity", queueCapacity)
}

func (t *readAheadTrace) observeQueueDepth(depth int) {
	for old := t.peakQueue.Load(); int64(depth) > old; old = t.peakQueue.Load() {
		if t.peakQueue.CompareAndSwap(old, int64(depth)) {
			return
		}
	}
}

type rawEvent struct {
	action  watch.EventType
	payload []byte
	err     error
}

type decodeJob struct {
	event    rawEvent
	response chan decodeResult
}

type decodeResult struct {
	action       watch.EventType
	object       runtime.Object
	err          error
	closeOnError bool
}

// ReadAheadDecoder overlaps envelope reading with bounded concurrent object decoding.
// Decode must be called by only one goroutine.
type ReadAheadDecoder struct {
	stream     streaming.Decoder
	object     runtime.Decoder
	queue      chan rawEvent
	jobs       chan decodeJob
	responses  chan chan decodeResult
	decoded    chan decodeResult
	done       chan struct{}
	readDone   chan struct{}
	decodeDone chan struct{}
	traceDone  chan struct{}
	once       sync.Once
	traceOnce  sync.Once
	trace      *readAheadTrace

	terminal error
}

// NewReadAheadDecoder starts one envelope reader. The caller must close the
// decoder when it stops consuming events.
func NewReadAheadDecoder(stream streaming.Decoder, object runtime.Decoder) *ReadAheadDecoder {
	return NewReadAheadDecoderWithLogger(klog.Background(), stream, object)
}

// NewReadAheadDecoderWithLogger records stage timings for the opt-in decoder.
func NewReadAheadDecoderWithLogger(logger klog.Logger, stream streaming.Decoder, object runtime.Decoder) *ReadAheadDecoder {
	return NewReadAheadDecoderWithLoggerAndQueueLength(logger, stream, object, 0)
}

// NewReadAheadDecoderWithLoggerAndQueueLength records stage timings and uses queueLength
// as the raw event queue capacity. A non-positive queueLength uses the default capacity.
func NewReadAheadDecoderWithLoggerAndQueueLength(logger klog.Logger, stream streaming.Decoder, object runtime.Decoder, queueLength int) *ReadAheadDecoder {
	if queueLength <= 0 {
		queueLength = defaultReadAheadBufferSize
	}
	d := &ReadAheadDecoder{
		stream:     stream,
		object:     object,
		queue:      make(chan rawEvent, queueLength),
		jobs:       make(chan decodeJob, readAheadDecodeConcurrency),
		responses:  make(chan chan decodeResult, readAheadDecodeConcurrency-1),
		decoded:    make(chan decodeResult, readAheadDecodeConcurrency),
		done:       make(chan struct{}),
		readDone:   make(chan struct{}),
		decodeDone: make(chan struct{}),
		traceDone:  make(chan struct{}),
	}
	d.trace = &readAheadTrace{logger: logger}
	go d.read()
	go d.decode()
	go d.reportTrace()
	return d
}

// decode runs a bounded pool of object decoders and emits results in input
// order. Envelope decoding remains single-threaded because streaming.Decoder
// owns the stream framing state.
func (d *ReadAheadDecoder) decode() {
	defer func() {
		close(d.decoded)
		close(d.decodeDone)
	}()
	var workers sync.WaitGroup
	workers.Add(readAheadDecodeConcurrency)
	for i := 0; i < readAheadDecodeConcurrency; i++ {
		go func() {
			defer workers.Done()
			for job := range d.jobs {
				active := d.trace.workerActive.Add(1)
				for old := d.trace.peakWorkers.Load(); active > old; old = d.trace.peakWorkers.Load() {
					if d.trace.peakWorkers.CompareAndSwap(old, active) {
						break
					}
				}
				result := d.decodeEvent(job.event)
				d.trace.workerActive.Add(-1)
				job.response <- result
			}
		}()
	}

	go func() {
		defer close(d.jobs)
		defer close(d.responses)
		for {
			select {
			case <-d.done:
				return
			case event, ok := <-d.queue:
				if !ok {
					return
				}
				response := make(chan decodeResult, 1)
				select {
				case <-d.done:
					return
				case d.responses <- response:
				}
				started := time.Now()
				d.trace.workerQueueStart.Store(started.UnixNano())
				select {
				case <-d.done:
					d.trace.workerQueueStart.Store(0)
					return
				case d.jobs <- decodeJob{event: event, response: response}:
				}
				d.trace.workerQueueNanos.Add(time.Since(started).Nanoseconds())
				d.trace.workerQueueStart.Store(0)
			}
		}
	}()

	for response := range d.responses {
		var result decodeResult
		started := time.Now()
		d.trace.resultWaitStart.Store(started.UnixNano())
		select {
		case <-d.done:
			d.trace.resultWaitStart.Store(0)
			return
		case result = <-response:
		}
		d.trace.resultWaitNanos.Add(time.Since(started).Nanoseconds())
		d.trace.resultWaitStart.Store(0)
		select {
		case <-d.done:
			return
		case d.decoded <- result:
		}
		if result.err != nil {
			return
		}
	}

}

func (d *ReadAheadDecoder) decodeEvent(event rawEvent) decodeResult {
	if event.err != nil {
		return decodeResult{err: event.err}
	}

	started := time.Now()
	if d.trace.objectActive.Add(1) == 1 {
		d.trace.objectStart.Store(started.UnixNano())
	}
	obj, err := runtime.Decode(d.object, event.payload)
	d.trace.objects.Add(1)
	d.trace.objectNanos.Add(time.Since(started).Nanoseconds())
	if d.trace.objectActive.Add(-1) == 0 {
		d.trace.objectStart.Store(0)
	}
	if err != nil {
		return decodeResult{err: fmt.Errorf("unable to decode watch event: %v", err), closeOnError: true}
	}
	return decodeResult{action: event.action, object: obj}
}

// StreamWatcherTraceEnabled keeps StreamWatcher timing logs limited to opt-in read-ahead streams.
func (d *ReadAheadDecoder) StreamWatcherTraceEnabled() bool {
	return true
}

func (d *ReadAheadDecoder) reportTrace() {
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

func (d *ReadAheadDecoder) reportSnapshot(phase string) {
	if reporter, ok := d.stream.(interface{ ReportTrace(string) }); ok {
		reporter.ReportTrace(phase)
	}
	d.trace.log(phase, len(d.queue), cap(d.queue))
}

func (d *ReadAheadDecoder) read() {
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
			// A terminal error must not retain a partially decoded payload.
			event = rawEvent{err: err}
		}
		select {
		case <-d.done:
			return
		default:
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

// Decode returns the next fully decoded event in stream order.
func (d *ReadAheadDecoder) Decode() (watch.EventType, runtime.Object, error) {
	select {
	case <-d.done:
		return "", nil, errReadAheadClosed
	default:
	}
	var ok bool
	started := time.Now()
	d.trace.consumerStart.Store(started.UnixNano())
	select {
	case result, resultOK := <-d.decoded:
		ok = resultOK
		if !ok {
			err := d.terminal
			if err == nil {
				err = errReadAheadExhausted
			}
			return "", nil, err
		}
		d.trace.consumerWaitNanos.Add(time.Since(started).Nanoseconds())
		d.trace.consumerStart.Store(0)
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

// Close stops the reader and releases queued raw payloads.
func (d *ReadAheadDecoder) Close() {
	d.once.Do(func() {
		close(d.done)
		d.stream.Close()
	})
	<-d.readDone
	<-d.decodeDone
	<-d.traceDone
	// The reader has stopped sending. A concurrent Decode may own one entry;
	// draining receives only the entries still held by the queue.
	for range d.queue {
	}
	d.traceOnce.Do(func() {
		d.reportSnapshot("final")
	})
}
