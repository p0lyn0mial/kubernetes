package openshiftkubeapiserver

import (
	"net/http"
	"strings"
	"sync"
	"time"
	"context"

	authenticationv1 "k8s.io/api/authentication/v1"
	genericapiserver "k8s.io/apiserver/pkg/server"
	patchfilters "k8s.io/kubernetes/openshift-kube-apiserver/filters"
	"k8s.io/kubernetes/openshift-kube-apiserver/filters/deprecatedapirequest"
	"k8s.io/apiserver/pkg/endpoints/request"

	authorizationv1 "github.com/openshift/api/authorization/v1"
	"github.com/openshift/library-go/pkg/apiserver/httprequest"
)

// TODO switch back to taking a kubeapiserver config.  For now make it obviously safe for 3.11
func BuildHandlerChain(consolePublicURL string, oauthMetadataFile string, deprecatedAPIRequestController deprecatedapirequest.APIRequestLogger) (func(apiHandler http.Handler, kc *genericapiserver.Config) http.Handler, error) {
	// load the oauthmetadata when we can return an error
	oAuthMetadata := []byte{}
	if len(oauthMetadataFile) > 0 {
		var err error
		oAuthMetadata, err = loadOAuthMetadataFile(oauthMetadataFile)
		if err != nil {
			return nil, err
		}
	}

	return func(apiHandler http.Handler, genericConfig *genericapiserver.Config) http.Handler {
			// well-known comes after the normal handling chain. This shows where to connect for oauth information
			handler := withOAuthInfo(apiHandler, oAuthMetadata)

			// after normal chain, so that we have request info
			handler = withLongRunningRequestTermination(handler, genericConfig)

			// after normal chain, so that user is in context
			handler = patchfilters.WithDeprecatedApiRequestLogging(handler, deprecatedAPIRequestController)

			// this is the normal kube handler chain
			handler = genericapiserver.DefaultBuildHandlerChain(handler, genericConfig)

			// these handlers are all before the normal kube chain
			handler = translateLegacyScopeImpersonation(handler)

			// redirects from / and /console to consolePublicURL if you're using a browser
			handler = withConsoleRedirect(handler, consolePublicURL)

			return handler
		},

		nil
}

// If we know the location of the asset server, redirect to it when / is requested
// and the Accept header supports text/html
func withOAuthInfo(handler http.Handler, oAuthMetadata []byte) http.Handler {
	if len(oAuthMetadata) == 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != oauthMetadataEndpoint {
			// Dispatch to the next handler
			handler.ServeHTTP(w, req)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(oAuthMetadata)
	})
}

// If we know the location of the asset server, redirect to it when / is requested
// and the Accept header supports text/html
func withConsoleRedirect(handler http.Handler, consolePublicURL string) http.Handler {
	if len(consolePublicURL) == 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/console") ||
			(req.URL.Path == "/" && httprequest.PrefersHTML(req)) {
			http.Redirect(w, req, consolePublicURL, http.StatusFound)
			return
		}
		// Dispatch to the next handler
		handler.ServeHTTP(w, req)
	})
}

// withLongRunningRequestTermination starts closing long running requests upon receiving ShutDownInProgressCh signal.
//
// It does it by running two additional go routines.
// One for running the request and the second one for intercepting the termination signal and propagating it to the requests.
// If the request is not terminated within 5 seconds it will be forcefully killed.
//
// This filter exists because sometimes propagating the termination signal is not enough.
// It turned out that long running requests might block on:
//  io.Read() for example in https://golang.org/src/net/http/httputil/reverseproxy.go
//  http2.(*serverConn).writeDataFromHandler which might be actually an issue with the std lib itself
//
// Instead of trying to identify current and future issues we provide a filter that ensures terminating long running requests.
//
// Also note that upon receiving termination signal the http server sends
// sends GOAWAY with ErrCodeNo to tell the client we're gracefully shutting down.
// But the connection isn't closed until all current streams are done.
func withLongRunningRequestTermination(handler http.Handler, genericConfig *genericapiserver.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestInfo, found := request.RequestInfoFrom(req.Context())
		if !found {
			handler.ServeHTTP(w, req)
			return
		}

		// TODO: add openshift specific requests
		// TODO: can LongRunningFunc be nil?
		isLongRunning := genericConfig.LongRunningFunc(req, requestInfo)
		if !isLongRunning {
			handler.ServeHTTP(w, req)
			return
		}

		ctx, cancelCtxFn := context.WithCancel(req.Context())
		defer cancelCtxFn()
		req = req.WithContext(ctx)

		var wg sync.WaitGroup
		wg.Add(2)
		errCh := make(chan interface{})
		defer close(errCh)
		doneCh := make(chan struct{})

		go func() {
			defer wg.Done()
			defer func() {
				err := recover()
				select {
				case errCh <- err:
					return
				case <-doneCh:
					return
				}
			}()
			select {
			case <-genericConfig.TerminationStartCh:
				cancelCtxFn()
				time.Sleep(5 * time.Second)
				panic(http.ErrAbortHandler)
			case <-doneCh:
				return
			}
		}()

		go func() {
			defer wg.Done()
			defer func() {
				err := recover()
				select {
				case errCh <- err:
					return
				case <-doneCh:
					return
				}
			}()
			handler.ServeHTTP(w, req)
		}()

		err := <-errCh
		close(doneCh)
		wg.Wait()
		if err != nil {
			panic(err)
		}
	})
}

// legacyImpersonateUserScopeHeader is the header name older servers were using
// just for scopes, so we need to translate it from clients that may still be
// using it.
const legacyImpersonateUserScopeHeader = "Impersonate-User-Scope"

// translateLegacyScopeImpersonation is a filter that will translates user scope impersonation for openshift into the equivalent kube headers.
func translateLegacyScopeImpersonation(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for _, scope := range req.Header[legacyImpersonateUserScopeHeader] {
			req.Header[authenticationv1.ImpersonateUserExtraHeaderPrefix+authorizationv1.ScopesKey] =
				append(req.Header[authenticationv1.ImpersonateUserExtraHeaderPrefix+authorizationv1.ScopesKey], scope)
		}

		handler.ServeHTTP(w, req)
	})
}
