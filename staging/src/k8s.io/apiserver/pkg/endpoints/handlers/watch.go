/*
Copyright 2014 The Kubernetes Authors.

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

package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/apiserver/pkg/util/wsstream"
	"k8s.io/klog/v2"

	"golang.org/x/net/websocket"
)

// nothing will ever be sent down this channel
var neverExitWatch <-chan time.Time = make(chan time.Time)

// timeoutFactory abstracts watch timeout logic for testing
type TimeoutFactory interface {
	TimeoutCh() (<-chan time.Time, func() bool)
}

// realTimeoutFactory implements timeoutFactory
type realTimeoutFactory struct {
	timeout time.Duration
}

// TimeoutCh returns a channel which will receive something when the watch times out,
// and a cleanup function to call when this happens.
func (w *realTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	if w.timeout == 0 {
		return neverExitWatch, func() bool { return false }
	}
	t := time.NewTimer(w.timeout)
	return t.C, t.Stop
}

// serveWatch will serve a watch response.
// TODO: the functionality in this method and in WatchServer.Serve is not cleanly decoupled.
func serveWatch(watcher watch.Interface, scope *RequestScope, mediaTypeOptions negotiation.MediaTypeOptions, req *http.Request, w http.ResponseWriter, timeout time.Duration) {
	defer watcher.Stop()

	options, err := optionsForTransform(mediaTypeOptions, req)
	if err != nil {
		scope.err(err, w, req)
		return
	}

	// negotiate for the stream serializer from the scope's serializer
	serializer, err := negotiation.NegotiateOutputMediaTypeStream(req, scope.Serializer, scope)
	if err != nil {
		scope.err(err, w, req)
		return
	}
	framer := serializer.StreamSerializer.Framer
	streamSerializer := serializer.StreamSerializer.Serializer
	encoder := scope.Serializer.EncoderForVersion(streamSerializer, scope.Kind.GroupVersion())
	useTextFraming := serializer.EncodesAsText
	if framer == nil {
		scope.err(fmt.Errorf("no framer defined for %q available for embedded encoding", serializer.MediaType), w, req)
		return
	}
	// TODO: next step, get back mediaTypeOptions from negotiate and return the exact value here
	mediaType := serializer.MediaType
	if mediaType != runtime.ContentTypeJSON {
		mediaType += ";stream=watch"
	}

	// locate the appropriate embedded encoder based on the transform
	var embeddedEncoder runtime.Encoder
	contentKind, contentSerializer, transform := targetEncodingForTransform(scope, mediaTypeOptions, req)
	if transform {
		info, ok := runtime.SerializerInfoForMediaType(contentSerializer.SupportedMediaTypes(), serializer.MediaType)
		if !ok {
			scope.err(fmt.Errorf("no encoder for %q exists in the requested target %#v", serializer.MediaType, contentSerializer), w, req)
			return
		}
		embeddedEncoder = contentSerializer.EncoderForVersion(info.Serializer, contentKind.GroupVersion())
	} else {
		embeddedEncoder = scope.Serializer.EncoderForVersion(serializer.Serializer, contentKind.GroupVersion())
	}

	ctx := req.Context()

	server := &WatchServer{
		Watching: watcher,
		Scope:    scope,

		UseTextFraming:  useTextFraming,
		MediaType:       mediaType,
		Framer:          framer,
		Encoder:         encoder,
		EmbeddedEncoder: embeddedEncoder,

		Fixup: func(obj runtime.Object) runtime.Object {
			result, err := transformObject(ctx, obj, options, mediaTypeOptions, scope, req)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to transform object %v: %v", reflect.TypeOf(obj), err))
				return obj
			}
			// When we are transformed to a table, use the table options as the state for whether we
			// should print headers - on watch, we only want to print table headers on the first object
			// and omit them on subsequent events.
			if tableOptions, ok := options.(*metav1.TableOptions); ok {
				tableOptions.NoHeaders = true
			}
			return result
		},

		TimeoutFactory: &realTimeoutFactory{timeout},
	}

	server.ServeHTTP(w, req)
}

// WatchServer serves a watch.Interface over a websocket or vanilla HTTP.
type WatchServer struct {
	Watching watch.Interface
	Scope    *RequestScope

	// true if websocket messages should use text framing (as opposed to binary framing)
	UseTextFraming bool
	// the media type this watch is being served with
	MediaType string
	// used to frame the watch stream
	Framer runtime.Framer
	// used to encode the watch stream event itself
	Encoder runtime.Encoder
	// used to encode the nested object in the watch stream
	EmbeddedEncoder runtime.Encoder
	// used to correct the object before we send it to the serializer
	Fixup func(runtime.Object) runtime.Object

	TimeoutFactory TimeoutFactory

	url string
}

// ServeHTTP serves a series of encoded events via HTTP with Transfer-Encoding: chunked
// or over a websocket connection.
func (s *WatchServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	//ctx, cancelCtxFn := context.WithCancel(req.Context())
	//req = req.WithContext(ctx)
	//defer cancelCtxFn()

	kind := s.Scope.Kind
	metrics.RegisteredWatchers.WithContext(req.Context()).WithLabelValues(kind.Group, kind.Version, kind.Kind).Inc()
	defer metrics.RegisteredWatchers.WithContext(req.Context()).WithLabelValues(kind.Group, kind.Version, kind.Kind).Dec()

	w = httplog.Unlogged(req, w)
	s.url = req.URL.String()

	if s.Scope.ShutDownInProgressCh == nil {
		klog.Infof("Termination: watch stop chan is nil for %v gv %v", req.URL, s.Scope.Kind.GroupVersion().String())
	}

	if wsstream.IsWebSocketRequest(req) {
		klog.Infof("WebSocket: url %v", req.URL)
		w.Header().Set("Content-Type", s.MediaType)
		websocket.Handler(s.HandleWS).ServeHTTP(w, req)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("unable to start watch - can't get http.Flusher: %#v", w)
		utilruntime.HandleError(err)
		s.Scope.err(errors.NewInternalError(err), w, req)
		return
	}

	framer := s.Framer.NewFrameWriter(w)
	if framer == nil {
		// programmer error
		err := fmt.Errorf("no stream framing support is available for media type %q", s.MediaType)
		utilruntime.HandleError(err)
		s.Scope.err(errors.NewBadRequest(err.Error()), w, req)
		return
	}
	e := streaming.NewEncoder(framer, s.Encoder)

	// ensure the connection times out
	timeoutCh, cleanup := s.TimeoutFactory.TimeoutCh()
	defer cleanup()

	// begin the stream
	w.Header().Set("Content-Type", s.MediaType)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var unknown runtime.Unknown
	internalEvent := &metav1.InternalEvent{}
	outEvent := &metav1.WatchEvent{}
	buf := &bytes.Buffer{}
	ch := s.Watching.ResultChan()




	done := req.Context().Done()
	shutdownInProgressCh := s.Scope.ShutDownInProgressCh


	//ua := req.Header.Get("User-Agent")

	/*
	defer func() {
		klog.Infof("Termination: watch ended for %v, ua %v", req.URL, ua)
	}()
	klog.Infof("Termination watch started for %v, ua %v", req.URL, ua)*/
	/*go func () {
		select {
		case <-shutdownInProgressCh:
			//cancelCtxFn()
			//klog.Infof("Termination: watch signal received (go) canceling for %v, ua %v", req.URL, ua)
			select {
			case <-done:
				return
			case <-timeoutCh:
				return
			case <-time.After(5 * time.Second):
				//klog.Infof("Termination: watch killing for %v, ua %v", req.URL, ua)
				panic(http.ErrAbortHandler)
			}
		case <-done:
			return
		case <-timeoutCh:
			return
		}
	}()*/

	for {
		select {
		case <-shutdownInProgressCh:
			return
		case <-done:
			return
		case <-timeoutCh:
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			metrics.WatchEvents.WithContext(req.Context()).WithLabelValues(kind.Group, kind.Version, kind.Kind).Inc()

			obj := s.Fixup(event.Object)
			if err := s.EmbeddedEncoder.Encode(obj, buf); err != nil {
				// unexpected error
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object %T: %v", obj, err))
				return
			}

			// ContentType is not required here because we are defaulting to the serializer
			// type
			unknown.Raw = buf.Bytes()
			event.Object = &unknown
			metrics.WatchEventsSizes.WithContext(req.Context()).WithLabelValues(kind.Group, kind.Version, kind.Kind).Observe(float64(len(unknown.Raw)))

			*outEvent = metav1.WatchEvent{}

			// create the external type directly and encode it.  Clients will only recognize the serialization we provide.
			// The internal event is being reused, not reallocated so its just a few extra assignments to do it this way
			// and we get the benefit of using conversion functions which already have to stay in sync
			*internalEvent = metav1.InternalEvent(event)
			err := metav1.Convert_v1_InternalEvent_To_v1_WatchEvent(internalEvent, outEvent, nil)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to convert watch object: %v", err))
				// client disconnect.
				return
			}

			if err := e.Encode(outEvent); err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object %T: %v (%#v)", outEvent, err, e))
				// client disconnect.
				return
			}
			if len(ch) == 0 {
				flusher.Flush()
			}

			buf.Reset()
		}
	}
}

// HandleWS implements a websocket handler.
func (s *WatchServer) HandleWS(ws *websocket.Conn) {
	defer ws.Close()
	done := make(chan struct{})

	go func() {
		defer utilruntime.HandleCrash()
		// This blocks until the connection is closed.
		// Client should not send anything.
		wsstream.IgnoreReceives(ws, 0)
		// Once the client closes, we should also close
		close(done)
	}()

	var unknown runtime.Unknown
	internalEvent := &metav1.InternalEvent{}
	buf := &bytes.Buffer{}
	streamBuf := &bytes.Buffer{}
	ch := s.Watching.ResultChan()
	shutdownInProgressCh := s.Scope.ShutDownInProgressCh

	for {
		select {
		case <-shutdownInProgressCh:
			klog.Infof("Termination: websocket closing for %v", s.url)
			return
		case <-done:
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			obj := s.Fixup(event.Object)
			if err := s.EmbeddedEncoder.Encode(obj, buf); err != nil {
				// unexpected error
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object %T: %v", obj, err))
				return
			}

			// ContentType is not required here because we are defaulting to the serializer
			// type
			unknown.Raw = buf.Bytes()
			event.Object = &unknown

			// the internal event will be versioned by the encoder
			// create the external type directly and encode it.  Clients will only recognize the serialization we provide.
			// The internal event is being reused, not reallocated so its just a few extra assignments to do it this way
			// and we get the benefit of using conversion functions which already have to stay in sync
			outEvent := &metav1.WatchEvent{}
			*internalEvent = metav1.InternalEvent(event)
			err := metav1.Convert_v1_InternalEvent_To_v1_WatchEvent(internalEvent, outEvent, nil)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to convert watch object: %v", err))
				// client disconnect.
				return
			}
			if err := s.Encoder.Encode(outEvent, streamBuf); err != nil {
				// encoding error
				utilruntime.HandleError(fmt.Errorf("unable to encode event: %v", err))
				return
			}
			if s.UseTextFraming {
				if err := websocket.Message.Send(ws, streamBuf.String()); err != nil {
					// Client disconnect.
					return
				}
			} else {
				if err := websocket.Message.Send(ws, streamBuf.Bytes()); err != nil {
					// Client disconnect.
					return
				}
			}
			buf.Reset()
			streamBuf.Reset()
		}
	}
}
