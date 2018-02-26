/*
Copyright 2018 The Kubernetes Authors.

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

package testing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	fakedisco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	fakerest "k8s.io/client-go/rest/fake"
	"k8s.io/client-go/scale"
	testcore "k8s.io/client-go/testing"
)

func FakeScaleClient(discoveryResources []*metav1.APIResourceList, pathsResources map[string]runtime.Object, pathsOnError map[string]map[string]kerrors.StatusError) (scale.ScalesGetter, error) {
	fakeDiscoveryClient := &fakedisco.FakeDiscovery{Fake: &testcore.Fake{}}
	fakeDiscoveryClient.Resources = discoveryResources
	restMapperRes, err := discovery.GetAPIGroupResources(fakeDiscoveryClient)
	if err != nil {
		return nil, err
	}
	restMapper := discovery.NewRESTMapper(restMapperRes, apimeta.InterfacesForUnstructured)
	codecs := serializer.NewCodecFactory(scale.NewScaleConverter().Scheme())
	fakeReqHandler := func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		scale, isScalePath := pathsResources[path]
		if !isScalePath {
			return nil, fmt.Errorf("unexpected request for URL %q with method %q", req.URL.String(), req.Method)
		}

		shouldReturnAnError := func(verb string) ([]byte, int, bool) {
			if errors, errorsExists := pathsOnError[path]; errorsExists {
				if anError, anErrorExists := errors[verb]; anErrorExists {
					encodedError, err := json.Marshal(anError)
					if err != nil {
						return nil, 0, false
					}
					return encodedError, int(anError.ErrStatus.Code), true
				}
			}
			return nil, 0, false
		}

		switch req.Method {
		case "GET":
			if anError, httpCode, should := shouldReturnAnError("GET"); should {
				return &http.Response{Status: "Failure", StatusCode: httpCode, Header: defaultHeaders(), Body: bytesBody(anError)}, nil
			}
			res, err := json.Marshal(scale)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Header: defaultHeaders(), Body: bytesBody(res)}, nil
		case "PUT":
			if anError, httpCode, should := shouldReturnAnError("PUT"); should {
				return &http.Response{Status: "Failure", StatusCode: httpCode, Header: defaultHeaders(), Body: bytesBody(anError)}, nil
			}
			decoder := codecs.UniversalDeserializer()
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			newScale, newScaleGVK, err := decoder.Decode(body, nil, nil)
			if err != nil {
				return nil, fmt.Errorf("unexpected request body: %v", err)
			}
			if *newScaleGVK != scale.GetObjectKind().GroupVersionKind() {
				return nil, fmt.Errorf("unexpected scale API version %s (expected %s)", newScaleGVK.String(), scale.GetObjectKind().GroupVersionKind().String())
			}
			res, err := json.Marshal(newScale)
			if err != nil {
				return nil, err
			}

			pathsResources[path] = newScale
			return &http.Response{StatusCode: 200, Header: defaultHeaders(), Body: bytesBody(res)}, nil
		default:
			return nil, fmt.Errorf("unexpected request for URL %q with method %q", req.URL.String(), req.Method)
		}
	}

	fakeClient := &fakerest.RESTClient{
		Client: fakerest.CreateHTTPClient(fakeReqHandler),
		NegotiatedSerializer: serializer.DirectCodecFactory{
			CodecFactory: serializer.NewCodecFactory(scale.NewScaleConverter().Scheme()),
		},
		GroupVersion:     schema.GroupVersion{},
		VersionedAPIPath: "/not/a/real/path",
	}

	resolver := scale.NewDiscoveryScaleKindResolver(fakeDiscoveryClient)
	client := scale.New(fakeClient, restMapper, dynamic.LegacyAPIPathResolverFunc, resolver)
	return client, nil
}

func bytesBody(bodyBytes []byte) io.ReadCloser {
	return ioutil.NopCloser(bytes.NewReader(bodyBytes))
}

func defaultHeaders() http.Header {
	header := http.Header{}
	header.Set("Content-Type", runtime.ContentTypeJSON)
	return header
}
