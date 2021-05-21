/*
Copyright 2021 The Kubernetes Authors.

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

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"golang.org/x/net/http2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apitesting "k8s.io/apiserver/pkg/endpoints/testing"
	genericapitesting "k8s.io/apiserver/pkg/endpoints/testing"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

var (
	// startServerShutdown is a signal from the backend after receiving all (25) requests
	// after which the test shuts down the HTTP server
	startServerShutdown = make(chan struct{})

	// handlerLock used in the backendHTTPHandler to count the number of requests and signal the test that the termination can start
	handlerLock = sync.Mutex{}
)

var backendCrt = []byte(`-----BEGIN CERTIFICATE-----
MIIDTjCCAjagAwIBAgIJANYWBFaLyBC/MA0GCSqGSIb3DQEBCwUAMFAxCzAJBgNV
BAYTAlBMMQ8wDQYDVQQIDAZQb2xhbmQxDzANBgNVBAcMBkdkYW5zazELMAkGA1UE
CgwCU0sxEjAQBgNVBAMMCTEyNy4wLjAuMTAeFw0yMDEyMTExMDI0MzBaFw0zMDEy
MDkxMDI0MzBaMFAxCzAJBgNVBAYTAlBMMQ8wDQYDVQQIDAZQb2xhbmQxDzANBgNV
BAcMBkdkYW5zazELMAkGA1UECgwCU0sxEjAQBgNVBAMMCTEyNy4wLjAuMTCCASIw
DQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAMYax2q/m/N237UFMFKZsox4EyKq
De+mbaRGeKqnI7Gi9Ai3b7BPCIa7RFJ2ntpGUd5GyL+HCQHG8/f6DjsbUuhZnmn7
F7ZJeih2DP2acKkODdGbXA52kABCMdDs2DMYhR2UwECY2t+DLpxqJqE2ab8pI9Xd
BZ3pCNodS03yHXzfeJV44lCjxoDOi9ynXLjd3w3+FowomHMEBunTepiqnbgoYtnn
RW9tQyQQK5g6+/j/O1M8o71s/0loBT3vKSqNSrdlMOEGrj4yyL/Cw1NmQf1V1sGf
w1QAW5xk7Br5oh8h1D+oflGWV3Y3zluuZQnA9D+vFpjL0969oFedsgr4UU8CAwEA
AaMrMCkwCQYDVR0TBAIwADALBgNVHQ8EBAMCBaAwDwYDVR0RBAgwBocEfwAAATAN
BgkqhkiG9w0BAQsFAAOCAQEAWbOF7TOfGiC59S50okfcS7M4gwz2kcbqOftWzcA1
lT1qX6TWj7A4bVIOMAFK2tWNd4Omk6bnIAxTJdHB7b1hrBjkpt2krEGH1S8xeRRz
Gs62KQwehM3fMhLvYSEqOQMETZn9AjEigYm6ohCO5obG9Gkfz7uvuv9rbIetbAmm
YE9HdDv6qhCqtynpP2yad3v53idlrDnCIe9e4eKUD5uR/MIp9mEFgnMXR1m43/ya
DnmddSsjtzamVvI/+2Cqjb8qT8dMHZrCBK64UwSaJsUKzSeF6yNvZKQ1yfA/NrfV
P6gNULDOqtPgXFP4j+Z402gjYox1bGHjeDHh1OVSnr9jVw==
-----END CERTIFICATE-----`)

var backendKey = []byte(`-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDGGsdqv5vzdt+1
BTBSmbKMeBMiqg3vpm2kRniqpyOxovQIt2+wTwiGu0RSdp7aRlHeRsi/hwkBxvP3
+g47G1LoWZ5p+xe2SXoodgz9mnCpDg3Rm1wOdpAAQjHQ7NgzGIUdlMBAmNrfgy6c
aiahNmm/KSPV3QWd6QjaHUtN8h1833iVeOJQo8aAzovcp1y43d8N/haMKJhzBAbp
03qYqp24KGLZ50VvbUMkECuYOvv4/ztTPKO9bP9JaAU97ykqjUq3ZTDhBq4+Msi/
wsNTZkH9VdbBn8NUAFucZOwa+aIfIdQ/qH5Rlld2N85brmUJwPQ/rxaYy9PevaBX
nbIK+FFPAgMBAAECggEAKmdZABR7gSWUxN6TdVrIySB6mBTmXsG0/lDHS1/zV/aV
XbhGA+sm3BABk9UoM3iR1Y45MiXpW6QGXLH9kdFLccidC/pfHPmlWDvMlAwWyVjk
xFUI41+leyiwGRRZQrag57ALZshRMT6XH4vpMODAydY4gXKJ3T8gUe+rSsfkX/Hl
Ce59c8pDsV3NDy4WKy00lYZfTqBqHu10qy9W8/eVYf+RUt53nrygCesnFfmJx/P8
GnHnN06QbZdpgVgbU49u+BujkjFgKH/60Ct9A19o34upXvkPOaKbABZ4dL1lUrbo
e3L3vnSdgXh1oOsy/JyICmDG5M2b68h33YNa+qUEgQKBgQDs1rf1+hw75o7iDlnx
E46CPC+9DkDuisWLgbUyW5KHPgropPl80uqnRxmaWpYGU/Fgyml08orpduHIWxtU
0tMRKm2HoFRM010fAp3xWc/B4pt2pdRMMSjMle//4FmoNlcJ8+owmD+2eook9Qjm
qN1UsQllkSoH4zx4iI+HhDJnHwKBgQDWIdGmlZqaYGhsndkco9yK+gve6W80ik4J
qnjnv9ux28SBrlORn2zzfGcu5LkJw8Dp9yjZzVUiFT8VFsWVNNuJyFba227Qxrwz
Hb/qvd5l2DfXHk4poyMZThzg7cxkxlVaWUIBMoGynDxQZIOypc6WmTeEG5+9W4+w
NCuTKt6/0QKBgQCOgALftUUXpXmC+i+TpbixE5WFovXekRCbB8gGLKLVTLczk0+p
kx4s19LH1Ik/9XHeUutwuh5qqmTfMDIZr1/fjC+q0wTl1KbK6cAuX2NpvPbdRJmf
3lQ2BGELC+nmFAv6qQ/XfUOYf9JuuiBI6IGDW6HTwqwPYuIXg9MYLqpE8QKBgA/2
2YCH6szTnzVp10PxW4Ho/nWSBb5vCT5jPTxZ63EpJ09bxdM3hZHplm/CkaEOvRU0
XhFO46f02Y0i83waQrvU+dS7Q1nBV0qgTyybFzeUlSUulzk3dmhukGycjf59YuOn
f+pC77R3PW/o7oClJ+/GYIMy5AfkCaRjX1RLf+vhAoGBANJBi0ARkhwOWbnD2urA
0tPMURSYIZ+JW7ghMspbm1XV1NTreCB/llLNqUGQ7zLAmH+KyqJK8O37/oh3VHrV
6jp9pqrqmibtGEIpQi4D9IM8Zo9mc8GexCf0x+11mamC+ZXjT+bvLQzbcJGnG5CL
W+S7SneWTL09leh5ATNhog6s
-----END PRIVATE KEY-----`)



var testAPIGroup = "test.group"
var testAPIGroup2 = "test.group2"
var testInternalGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: runtime.APIVersionInternal}
var testGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: "version"}
var newGroupVersion = schema.GroupVersion{Group: testAPIGroup, Version: "version2"}
var testGroup2Version = schema.GroupVersion{Group: testAPIGroup2, Version: "version"}
var testInternalGroup2Version = schema.GroupVersion{Group: testAPIGroup2, Version: runtime.APIVersionInternal}
var prefix = "apis"

var grouplessGroupVersion = schema.GroupVersion{Group: "", Version: "v1"}
var grouplessInternalGroupVersion = schema.GroupVersion{Group: "", Version: runtime.APIVersionInternal}
var grouplessPrefix = "api"

var groupVersions = []schema.GroupVersion{grouplessGroupVersion, testGroupVersion, newGroupVersion}

var codec = codecs.LegacyCodec(groupVersions...)
var testCodec = codecs.LegacyCodec(testGroupVersion)
var newCodec = codecs.LegacyCodec(newGroupVersion)

var accessor = meta.NewAccessor()
var selfLinker runtime.SelfLinker = accessor
var admissionControl admission.Interface

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)

	// unnamed core group
	scheme.AddUnversionedTypes(grouplessGroupVersion, &metav1.Status{})
	metav1.AddToGroupVersion(scheme, grouplessGroupVersion)

	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

func addGrouplessTypes() {
	scheme.AddKnownTypes(grouplessGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{}, &metav1.ListOptions{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{})
	scheme.AddKnownTypes(grouplessInternalGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{})

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func addTestTypes() {
	scheme.AddKnownTypes(testGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testGroupVersion, &examplev1.Pod{})
	scheme.AddKnownTypes(testInternalGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testInternalGroupVersion, &example.Pod{})
	// Register SimpleXGSubresource in both testGroupVersion and testGroup2Version, and also their
	// their corresponding internal versions, to verify that the desired group version object is
	// served in the tests.
	scheme.AddKnownTypes(testGroup2Version, &genericapitesting.SimpleXGSubresource{})
	scheme.AddKnownTypes(testInternalGroup2Version, &genericapitesting.SimpleXGSubresource{})
	metav1.AddToGroupVersion(scheme, testGroupVersion)

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func addNewTestTypes() {
	scheme.AddKnownTypes(newGroupVersion,
		&genericapitesting.Simple{}, &genericapitesting.SimpleList{},
		&metav1.DeleteOptions{}, &genericapitesting.SimpleGetOptions{}, &genericapitesting.SimpleRoot{},
		&examplev1.Pod{},
	)
	metav1.AddToGroupVersion(scheme, newGroupVersion)

	utilruntime.Must(genericapitesting.RegisterConversions(scheme))
}

func init() {
	// Certain API objects are returned regardless of the contents of storage:
	// api.Status is returned in errors

	addGrouplessTypes()
	addTestTypes()
	addNewTestTypes()

	scheme.AddFieldLabelConversionFunc(grouplessGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
	scheme.AddFieldLabelConversionFunc(testGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
	scheme.AddFieldLabelConversionFunc(newGroupVersion.WithKind("Simple"),
		func(label, value string) (string, string, error) {
			return label, value, nil
		},
	)
}

// watchJSON defines the expected JSON wire equivalent of watch.Event
type watchJSON struct {
	Type   watch.EventType `json:"type,omitempty"`
	Object json.RawMessage `json:"object,omitempty"`
}


type fakeTimeoutFactory struct {
	timeoutCh chan time.Time
	done      chan struct{}
}

func (t *fakeTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	return t.timeoutCh, func() bool {
		defer close(t.done)
		return true
	}
}

// serveWatch will serve a watch response according to the watcher and watchServer.
// Before watchServer.ServeHTTP, an error may occur like k8s.io/apiserver/pkg/endpoints/handlers/watch.go#serveWatch does.
func serveWatch(watcher watch.Interface, watchServer *handlers.WatchServer, preServeErr error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		defer watcher.Stop()

		if preServeErr != nil {
			responsewriters.ErrorNegotiated(preServeErr, watchServer.Scope.Serializer, watchServer.Scope.Kind.GroupVersion(), w, req)
			return
		}

		watchServer.ServeHTTP(w, req)
	}
}

func TestGracefulShutdownForWatchRequest(t *testing.T) {
	watcher := watch.NewFake()
	timeoutCh := make(chan time.Time)
	done := make(chan struct{})

	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
	if !ok || info.StreamSerializer == nil {
		t.Fatal(info)
	}
	serializer := info.StreamSerializer

	// Setup a new watchserver
	watchServer := &handlers.WatchServer{
		Scope:    &handlers.RequestScope{},
		Watching: watcher,

		MediaType:       "testcase/json",
		Framer:          serializer.Framer,
		Encoder:         newCodec,
		EmbeddedEncoder: newCodec,

		Fixup:          func(obj runtime.Object) runtime.Object { return obj },
		TimeoutFactory: &fakeTimeoutFactory{timeoutCh, done},
	}


	// set up the backend server
	backendServer := httptest.NewUnstartedServer(serveWatch(watcher, watchServer, nil))
	backendServer.EnableHTTP2 = true
	backendCert, err := tls.X509KeyPair(backendCrt, backendKey)
	if err != nil {
		t.Fatalf("backend: invalid x509/key pair: %v", err)
	}
	backendServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{backendCert},
		NextProtos:   []string{http2.NextProtoTLS},
	}
	backendServer.StartTLS()
	defer backendServer.Close()

	// Setup a client
	dest, _ := url.Parse(backendServer.URL)
	dest.Path = "/" + prefix + "/" + newGroupVersion.Group + "/" + newGroupVersion.Version + "/simple"
	dest.RawQuery = "watch=true"

	// set up the client
	clientCACertPool := x509.NewCertPool()
	clientCACertPool.AppendCertsFromPEM(backendCrt)
	clientTLSConfig := &tls.Config{
		RootCAs:    clientCACertPool,
		NextProtos: []string{http2.NextProtoTLS},
	}
	req, _ := http.NewRequest("GET", dest.String(), nil)
	client := http.Client{}
	client.Transport = &http2.Transport{
		TLSClientConfig: clientTLSConfig,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	watcher.Add(&apitesting.Simple{TypeMeta: metav1.TypeMeta{APIVersion: newGroupVersion.String()}})

	// Make sure we can actually watch an endpoint
	decoder := json.NewDecoder(resp.Body)
	//go func() {
		//for {
			var got watchJSON
			err = decoder.Decode(&got)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			fmt.Println(fmt.Sprintf("got %v", got))
			//watcher.Add(&apitesting.Simple{TypeMeta: metav1.TypeMeta{APIVersion: newGroupVersion.String()}})
		//}
	//}()
	//time.Sleep(1 * time.Second)
	fmt.Println(fmt.Sprintf("%v: stopping HTTP server %v", time.Now(), time.Now()))
	ctx, cancel := context.WithTimeout(context.Background(), 3 * time.Minute)
	err = backendServer.Config.Shutdown(ctx)
	cancel()
	fmt.Println(fmt.Sprintf("%v: the HTTP server stopped, err = %v", err, time.Now()))

	// Timeout and check for leaks
	// close(timeoutCh)
	/* select {
	case <-done:
		eventCh := watcher.ResultChan()
		select {
		case _, opened := <-eventCh:
			if opened {
				t.Errorf("Watcher received unexpected event")
			}
			if !watcher.IsStopped() {
				t.Errorf("Watcher is not stopped")
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Leaked watch on timeout")
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("Failed to stop watcher after %s of timeout signal", wait.ForeverTestTimeout.String())
	}*/

	// Make sure we can't receive any more events through the timeout watch
	//err = decoder.Decode(&got)
	//if err != io.EOF {
		//t.Errorf("Unexpected non-error")
	//}
}


func TestGracefulShutdownForHijackedConnection(t *testing.T) {
	t.Skip()
	// set up the backend server
	backendHandler := &backendHTTPHandler{}
	backendServer := httptest.NewUnstartedServer(backendHandler)
	backendCert, err := tls.X509KeyPair(backendCrt, backendKey)
	if err != nil {
		t.Fatalf("backend: invalid x509/key pair: %v", err)
	}
	backendServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{backendCert},
	}
	backendServer.StartTLS()
	defer backendServer.Close()

	// set up the client
	clientCACertPool := x509.NewCertPool()
	clientCACertPool.AppendCertsFromPEM(backendCrt)
	clientTLSConfig := &tls.Config{
		RootCAs: clientCACertPool,
	}
	client := &http.Client{}
	client.Transport = &http.Transport{
		TLSClientConfig: clientTLSConfig,
	}

	// client request
	sendRequest := func(wg *sync.WaitGroup) {
		defer func() {
			wg.Done()
		}()

		// act and validate
		_, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d", backendServer.Listener.Addr().(*net.TCPAddr).Port))
		if err == nil {
			t.Error("expected to get an error since the server had been shut down before a rsp was sent")
		}

		return
	}

	// this function starts the graceful shutdown
	go func() {
		<-startServerShutdown // signal from the backend after receiving all (25) requests

		backendServer.Config.Shutdown(context.Background())
	}()

	wg := sync.WaitGroup{}
	wg.Add(25)
	for i := 0; i < 25; i++ {
		go sendRequest(&wg)
	}
	wg.Wait()

	// validate backendHandler
	if backendHandler.counter != 25 {
		t.Errorf("the target server haven't received all expected requests, expected 25, it got %d", backendHandler.counter)
	}
}

type backendHTTPHandler struct {
	counter int
}

func (b *backendHTTPHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	handlerLock.Lock()
	b.counter++
	if b.counter == 25 {
		startServerShutdown <- struct{}{}
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		handlerLock.Unlock()
		panic("the given ResponseWriter doesn't support http.Hijacker. Are you using the wrong protocol?")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	handlerLock.Unlock()

	time.Sleep(5 * time.Second)

	bufrw.Write([]byte("hello from the backend"))
	bufrw.Flush()
}
