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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/klog/v2"
)

// ConcurrentReadAheadDecoder is the ordered, concurrent object-decoding
// implementation used for watch-list responses. Keeping this constructor
// separate makes the serial and concurrent implementations easy to compare or
// switch independently.
type ConcurrentReadAheadDecoder = ReadAheadDecoder

func NewConcurrentReadAheadDecoder(stream streaming.Decoder, object runtime.Decoder) *ConcurrentReadAheadDecoder {
	return NewConcurrentReadAheadDecoderWithLoggerAndQueueLength(klog.Background(), stream, object, 0)
}

func NewConcurrentReadAheadDecoderWithLoggerAndQueueLength(logger klog.Logger, stream streaming.Decoder, object runtime.Decoder, queueLength int) *ConcurrentReadAheadDecoder {
	return NewConcurrentReadAheadDecoderWithLoggerAndQueueLengthAndConcurrency(logger, stream, object, queueLength, 0)
}

func NewConcurrentReadAheadDecoderWithLoggerAndQueueLengthAndConcurrency(logger klog.Logger, stream streaming.Decoder, object runtime.Decoder, queueLength, decodeConcurrency int) *ConcurrentReadAheadDecoder {
	return NewReadAheadDecoderWithLoggerAndQueueLengthAndConcurrency(logger, stream, object, queueLength, decodeConcurrency)
}
