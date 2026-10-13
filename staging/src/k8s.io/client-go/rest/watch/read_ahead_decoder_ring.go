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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/watch"
)

type ringDecodeSlot struct {
	// The job transfers envelope ownership to a worker; the ready token
	// transfers result ownership to the consumer before the slot is recycled.
	envelope metav1.WatchEvent
	err      error
	result   decodeResult
	ready    chan struct{}
}

// RingReadAheadDecoder decodes objects concurrently using a fixed ring of
// reusable envelopes and result channels. Decode must have a single caller;
// Close may be called concurrently. The object decoder must support concurrent
// calls, and the stream must give each envelope ownership of its raw payload.
//
// The pipeline allocates at construction, not per successful event. Allocations
// made by the supplied stream and object decoder remain their responsibility.
// Unlike ReadAheadDecoder, this implementation omits per-event timing traces.
type RingReadAheadDecoder struct {
	stream      streaming.Decoder
	object      runtime.Decoder
	slots       []ringDecodeSlot
	free        chan struct{}
	jobs        chan *ringDecodeSlot
	done        chan struct{}
	readDone    chan struct{}
	cleanupDone chan struct{}
	once        sync.Once
	next        int
	terminal    error
}

// NewRingReadAheadDecoder bounds outstanding events by queueLength plus
// decodeConcurrency. Non-positive arguments use the read-ahead defaults.
// Call Close when consumption stops. Close interrupts the stream immediately;
// an object Decode already in progress may finish afterwards.
func NewRingReadAheadDecoder(stream streaming.Decoder, object runtime.Decoder, queueLength, decodeConcurrency int) *RingReadAheadDecoder {
	if queueLength <= 0 {
		queueLength = defaultReadAheadBufferSize
	}
	if decodeConcurrency <= 0 {
		decodeConcurrency = defaultReadAheadDecodeConcurrency
	}
	capacity := queueLength + decodeConcurrency
	d := &RingReadAheadDecoder{
		stream:      stream,
		object:      object,
		slots:       make([]ringDecodeSlot, capacity),
		free:        make(chan struct{}, capacity),
		jobs:        make(chan *ringDecodeSlot, capacity),
		done:        make(chan struct{}),
		readDone:    make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	for i := range d.slots {
		d.slots[i].ready = make(chan struct{}, 1)
		d.free <- struct{}{}
	}
	var workers sync.WaitGroup
	workers.Add(decodeConcurrency)
	for i := 0; i < decodeConcurrency; i++ {
		go func() {
			defer workers.Done()
			d.work()
		}()
	}
	go d.read()
	go func() {
		// Cleanup waits for ownership to return from the reader and workers.
		// Receiving from ready also safely competes with a concurrent Decode.
		<-d.done
		<-d.readDone
		workers.Wait()
		for i := range d.slots {
			d.slots[i].envelope = metav1.WatchEvent{}
			d.slots[i].err = nil
			select {
			case <-d.slots[i].ready:
				d.slots[i].result = decodeResult{}
			default:
			}
		}
		for range d.jobs {
		}
		close(d.cleanupDone)
	}()
	return d
}

func (d *RingReadAheadDecoder) read() {
	defer close(d.readDone)
	defer close(d.jobs)
	next := 0
	for {
		select {
		case <-d.done:
			return
		case <-d.free:
		}
		select {
		case <-d.done:
			return
		default:
		}
		slot := &d.slots[next]
		res, _, err := d.stream.Decode(nil, &slot.envelope)
		if err == nil && res != &slot.envelope {
			err = fmt.Errorf("unable to decode to metav1.WatchEvent")
		}
		if err == nil {
			switch watch.EventType(slot.envelope.Type) {
			case watch.Added, watch.Modified, watch.Deleted, watch.Error, watch.Bookmark:
			default:
				err = fmt.Errorf("got invalid watch event type: %v", slot.envelope.Type)
			}
		}
		slot.err = err
		if err != nil {
			slot.envelope = metav1.WatchEvent{}
		}
		select {
		case <-d.done:
			return
		case d.jobs <- slot:
		}
		if err != nil {
			return
		}
		next++
		if next == len(d.slots) {
			next = 0
		}
	}
}

func (d *RingReadAheadDecoder) work() {
	for {
		select {
		case <-d.done:
			return
		default:
		}
		var slot *ringDecodeSlot
		select {
		case <-d.done:
			return
		case job, ok := <-d.jobs:
			if !ok {
				return
			}
			slot = job
		}
		result := decodeResult{err: slot.err}
		if result.err == nil {
			result.action = watch.EventType(slot.envelope.Type)
			result.object, result.err = runtime.Decode(d.object, slot.envelope.Object.Raw)
			if result.err != nil {
				result = decodeResult{err: fmt.Errorf("unable to decode watch event: %v", result.err), closeOnError: true}
			}
		}
		// Returned objects belong to the consumer and must never be pooled.
		// Release raw payloads before publishing completion and reusing slots.
		slot.envelope = metav1.WatchEvent{}
		slot.err = nil
		slot.result = result
		select {
		case <-d.done:
			slot.result = decodeResult{}
			return
		case slot.ready <- struct{}{}:
		}
	}
}

// Decode waits on the next slot in stream order, regardless of which worker
// finishes first. A slot is reused only after its result has been consumed.
func (d *RingReadAheadDecoder) Decode() (watch.EventType, runtime.Object, error) {
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
	case <-d.slots[d.next].ready:
		slot := &d.slots[d.next]
		result := slot.result
		slot.result = decodeResult{}
		d.next++
		if d.next == len(d.slots) {
			d.next = 0
		}
		d.free <- struct{}{}
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

// Close stops new work without waiting for an uncancellable object decoder.
// Pending payloads and results are released once those decodes return.
func (d *RingReadAheadDecoder) Close() {
	d.once.Do(func() {
		close(d.done)
		d.stream.Close()
	})
	<-d.readDone
}

var _ watch.Decoder = (*RingReadAheadDecoder)(nil)
