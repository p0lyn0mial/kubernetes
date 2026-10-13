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
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
)

// OrderedReadAheadDecoder follows the apiserver's concurrentOrderedEventProcessing:
// each event gets a goroutine and a buffered response channel, and a collector
// consumes those channels in stream order. Decode must have a single caller;
// Close may be called concurrently. The object decoder must support concurrent
// calls, and the stream must give each envelope ownership of its raw payload.
type OrderedReadAheadDecoder struct {
	stream          streaming.Decoder
	object          runtime.Decoder
	incoming        chan rawEvent
	processingQueue chan chan decodeResult
	decoded         chan decodeResult
	done            chan struct{}
	readDone        chan struct{}
	cleanupDone     chan struct{}
	once            sync.Once
	terminal        error
}

// NewOrderedReadAheadDecoder buffers queueLength raw events and allows up to
// decodeConcurrency outstanding object decodes. Non-positive arguments use the
// read-ahead defaults. Call Close when consumption stops; an object Decode
// already in progress may finish after Close returns.
func NewOrderedReadAheadDecoder(stream streaming.Decoder, object runtime.Decoder, queueLength, decodeConcurrency int) *OrderedReadAheadDecoder {
	if queueLength <= 0 {
		queueLength = defaultReadAheadBufferSize
	}
	if decodeConcurrency <= 0 {
		decodeConcurrency = defaultReadAheadDecodeConcurrency
	}
	d := &OrderedReadAheadDecoder{
		stream:   stream,
		object:   object,
		incoming: make(chan rawEvent, queueLength),
		// The collector owns one response outside the queue, just as in
		// concurrentOrderedEventProcessing.
		processingQueue: make(chan chan decodeResult, decodeConcurrency-1),
		decoded:         make(chan decodeResult),
		done:            make(chan struct{}),
		readDone:        make(chan struct{}),
		cleanupDone:     make(chan struct{}),
	}
	var workers sync.WaitGroup
	scheduleDone := make(chan struct{})
	collectDone := make(chan struct{})
	go d.read()
	go func() {
		defer close(scheduleDone)
		d.scheduleEventProcessing(&workers)
	}()
	go func() {
		defer close(collectDone)
		d.collectEventProcessing()
	}()
	go func() {
		<-d.done
		<-d.readDone
		// Scheduling must stop before Wait because it adds per-event workers.
		<-scheduleDone
		workers.Wait()
		<-collectDone
		for range d.incoming {
		}
		for response := range d.processingQueue {
			select {
			case <-response:
			default:
			}
		}
		close(d.cleanupDone)
	}()
	return d
}

func (d *OrderedReadAheadDecoder) read() {
	defer close(d.readDone)
	defer close(d.incoming)
	for {
		select {
		case <-d.done:
			return
		default:
		}
		var envelope metav1.WatchEvent
		res, _, err := d.stream.Decode(nil, &envelope)
		if err == nil && res != &envelope {
			err = fmt.Errorf("unable to decode to metav1.WatchEvent")
		}
		if err == nil {
			switch watch.EventType(envelope.Type) {
			case watch.Added, watch.Modified, watch.Deleted, watch.Error, watch.Bookmark:
			default:
				err = fmt.Errorf("got invalid watch event type: %v", envelope.Type)
			}
		}
		event := rawEvent{action: watch.EventType(envelope.Type), payload: envelope.Object.Raw}
		if err != nil {
			// An envelope error must not retain a partially decoded payload.
			event = rawEvent{err: err}
		}
		select {
		case <-d.done:
			return
		case d.incoming <- event:
		}
		if err != nil {
			return
		}
	}
}

func (d *OrderedReadAheadDecoder) scheduleEventProcessing(workers *sync.WaitGroup) {
	defer close(d.processingQueue)
	for {
		var event rawEvent
		select {
		case <-d.done:
			return
		case e, ok := <-d.incoming:
			if !ok {
				return
			}
			event = e
		}
		response := make(chan decodeResult, 1)
		select {
		case <-d.done:
			return
		case d.processingQueue <- response:
		}
		workers.Add(1)
		go func(event rawEvent, response chan<- decodeResult) {
			defer workers.Done()
			result := decodeResult{err: event.err}
			if result.err == nil {
				result.action = event.action
				result.object, result.err = runtime.Decode(d.object, event.payload)
				if result.err != nil {
					result = decodeResult{err: fmt.Errorf("unable to decode watch event: %v", result.err), closeOnError: true}
				}
			}
			select {
			case <-d.done:
			case response <- result:
			}
		}(event, response)
	}
}

func (d *OrderedReadAheadDecoder) collectEventProcessing() {
	defer close(d.decoded)
	for {
		var response chan decodeResult
		select {
		case <-d.done:
			return
		case r, ok := <-d.processingQueue:
			if !ok {
				return
			}
			response = r
		}
		var result decodeResult
		select {
		case <-d.done:
			return
		case result = <-response:
		}
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

// Decode returns results and terminal errors in stream order.
func (d *OrderedReadAheadDecoder) Decode() (watch.EventType, runtime.Object, error) {
	select {
	case <-d.done:
		return "", nil, errReadAheadClosed
	default:
	}
	if d.terminal != nil {
		return "", nil, d.terminal
	}
	select {
	case <-d.done:
		return "", nil, errReadAheadClosed
	case result, ok := <-d.decoded:
		if !ok {
			return "", nil, errReadAheadExhausted
		}
		if result.err != nil {
			d.terminal = result.err
			if result.closeOnError {
				d.Close()
			}
			return "", nil, result.err
		}
		return result.action, result.object, nil
	}
}

// Close interrupts the stream without waiting for uncancellable object decodes.
// Queued payloads and results are released once those decodes finish.
func (d *OrderedReadAheadDecoder) Close() {
	d.once.Do(func() {
		close(d.done)
		d.stream.Close()
	})
	<-d.readDone
}

var _ watch.Decoder = (*OrderedReadAheadDecoder)(nil)
