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
	"io"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

const (
	batchReadAheadSize        = 16
	batchReadAheadConcurrency = 10
	batchReadAheadMaxFrame    = 16 * 1024 * 1024
)

type batchReadAheadDecoder struct {
	reader   io.ReadCloser
	envelope runtime.Decoder
	object   runtime.Decoder
	logger   klog.Logger

	batches   chan []rawEvent
	results   chan decodeResult
	done      chan struct{}
	closed    chan struct{}
	traceDone chan struct{}
	once      sync.Once
	traceOnce sync.Once
	trace     batchReadAheadTrace
	reporters []func(string)

	terminal error
}

type batchReadAheadTrace struct {
	batches           atomic.Int64
	frames            atomic.Int64
	bytes             atomic.Int64
	objectNanos       atomic.Int64
	queueWaitNanos    atomic.Int64
	consumerWaitNanos atomic.Int64
}

// NewBatchReadAheadDecoder reads and decodes WatchList response batches while
// retaining the one-event-at-a-time watch.Decoder contract.
func NewBatchReadAheadDecoder(logger klog.Logger, body io.ReadCloser, framer runtime.Framer, envelope, object runtime.Decoder, queueLength int, reporters ...func(string)) watch.Decoder {
	if queueLength <= 0 {
		queueLength = 100
	}
	reader := framer.NewFrameReader(body)
	d := &batchReadAheadDecoder{
		reader:    reader,
		envelope:  envelope,
		object:    object,
		logger:    logger,
		batches:   make(chan []rawEvent, max(1, queueLength/batchReadAheadSize)),
		results:   make(chan decodeResult, batchReadAheadSize*2),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
		traceDone: make(chan struct{}),
		reporters: reporters,
	}
	go d.read()
	go d.decode()
	go d.traceLoop()
	return d
}

func (d *batchReadAheadDecoder) traceLoop() {
	defer close(d.traceDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.reportTrace("periodic")
		case <-d.done:
			return
		}
	}
}

func (d *batchReadAheadDecoder) reportTrace(phase string) {
	d.logger.Info("WatchList read-ahead trace", "phase", phase, "decoder", "batch",
		"batches", d.trace.batches.Load(), "frames", d.trace.frames.Load(), "bytes", d.trace.bytes.Load(),
		"objectDecodeTime", time.Duration(d.trace.objectNanos.Load()),
		"batchQueueWaitTime", time.Duration(d.trace.queueWaitNanos.Load()),
		"consumerWaitTime", time.Duration(d.trace.consumerWaitNanos.Load()))
	for _, reporter := range d.reporters {
		reporter(phase)
	}
}

func (d *batchReadAheadDecoder) read() {
	defer close(d.batches)
	for {
		batch := make([]rawEvent, 0, batchReadAheadSize)
		for len(batch) < batchReadAheadSize {
			frame, err := readBatchFrame(d.reader)
			if err != nil {
				if err == io.EOF && len(batch) > 0 {
					batch = append(batch, rawEvent{err: io.EOF})
				} else if len(batch) == 0 {
					d.terminal = err
				}
				break
			}
			d.trace.frames.Add(1)
			d.trace.bytes.Add(int64(len(frame)))

			var got metav1.WatchEvent
			res, _, err := d.envelope.Decode(frame, nil, &got)
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
				batch = append(batch, rawEvent{err: err})
				break
			}
			batch = append(batch, rawEvent{action: watch.EventType(got.Type), payload: got.Object.Raw})
		}

		if len(batch) == 0 {
			return
		}
		d.trace.batches.Add(1)
		started := time.Now()
		select {
		case d.batches <- batch:
		case <-d.done:
			return
		}
		d.trace.queueWaitNanos.Add(time.Since(started).Nanoseconds())
		if batch[len(batch)-1].err != nil {
			return
		}
	}
}

func (d *batchReadAheadDecoder) decode() {
	defer close(d.results)
	defer close(d.closed)
	for batch := range d.batches {
		results := make([]decodeResult, len(batch))
		sem := make(chan struct{}, batchReadAheadConcurrency)
		var wg sync.WaitGroup
		for i := range batch {
			select {
			case <-d.done:
				return
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				results[i] = d.decodeEvent(batch[i])
			}(i)
		}
		wg.Wait()
		for _, result := range results {
			select {
			case d.results <- result:
			case <-d.done:
				return
			}
			if result.err != nil {
				return
			}
		}
	}
}

func (d *batchReadAheadDecoder) decodeEvent(event rawEvent) decodeResult {
	if event.err != nil {
		return decodeResult{err: event.err}
	}
	started := time.Now()
	obj, err := runtime.Decode(d.object, event.payload)
	d.trace.objectNanos.Add(time.Since(started).Nanoseconds())
	if err != nil {
		return decodeResult{err: fmt.Errorf("unable to decode watch event: %v", err), closeOnError: true}
	}
	return decodeResult{action: event.action, object: obj}
}

func (d *batchReadAheadDecoder) Decode() (watch.EventType, runtime.Object, error) {
	started := time.Now()
	select {
	case result, ok := <-d.results:
		d.trace.consumerWaitNanos.Add(time.Since(started).Nanoseconds())
		if !ok {
			if d.terminal != nil {
				return "", nil, d.terminal
			}
			return "", nil, io.EOF
		}
		if result.err != nil {
			if result.closeOnError {
				d.Close()
			}
			return "", nil, result.err
		}
		return result.action, result.object, nil
	case <-d.done:
		return "", nil, errReadAheadClosed
	}
}

func (d *batchReadAheadDecoder) Close() {
	d.once.Do(func() {
		close(d.done)
		_ = d.reader.Close()
	})
	<-d.closed
	<-d.traceDone
	d.traceOnce.Do(func() { d.reportTrace("final") })
}

func (d *batchReadAheadDecoder) StreamWatcherTraceEnabled() bool { return true }

func readBatchFrame(reader io.Reader) ([]byte, error) {
	buf := make([]byte, 1024)
	base := 0
	for {
		n, err := reader.Read(buf[base:])
		if err == io.ErrShortBuffer {
			if n == 0 || len(buf) >= batchReadAheadMaxFrame {
				return nil, streaming.ErrObjectTooLarge
			}
			base += n
			buf = append(buf, make([]byte, len(buf))...)
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:base+n], nil
	}
}

var _ watch.Decoder = (*batchReadAheadDecoder)(nil)
