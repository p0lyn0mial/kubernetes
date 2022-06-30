/*
Copyright 2022 The Kubernetes Authors.

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

package cache

import (
	"fmt"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// TestWatchListNoBookmark checks if the reflector won't be synced if no Bookmark event has been received
func TestWatchListNoBookmark(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent}}
	w, lw, s, r, stopCh := testData()

	go func() {
		w.Action(watch.Added, makePodFn("p1", "1"))
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, nil) // since the reflector didn't get a bookmark event we expect an empty store
}

// TestOldSemanticWhenWatchIsOff checks if the reflector uses the old LIST/WATCH semantics if the UseWatchList is turned off
func TestOldSemanticWhenWatchIsOff(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{{ResourceVersion: "1", AllowWatchBookmarks: true}}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()
	r.UseWatchList = false

	go func() {
		p := makePodFn("p1", "1")
		pods = append(pods, *p)
		w.Action(watch.Added, p)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 1)
	assertStore(t, s, pods)
}

// TestWatchListToleratesOnlyInvalidRequestError checks if returning any other error than apierrors.NewInvalid stops the reflector and reports the error
func TestWatchListToleratesOnlyInvalidRequestError(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent}}
	_, lw, s, r, stopCh := testData()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		return nil, fmt.Errorf("dummy errror")
	})

	if err := r.ListAndWatch(stopCh); err == nil {
		t.Fatal("expected to receive an error")
	}

	assertListCounter(t, lw, 0)
	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertStore(t, s, nil)
}

// TestWatchListFallbackToList checks if the reflector can fall back to old LIST/WATCH semantics when a server doesn't support streaming
func TestWatchListFallbackToList(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "1", AllowWatchBookmarks: true},
	}
	m := sync.Mutex{}
	w, lw, s, r, stopCh := testData()

	pods := []v1.Pod{*makePodFn("p1", "1")}
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		if options.ResourceVersionMatch == metav1.ResourceVersionMatchMostRecent {
			return nil, apierrors.NewInvalid(schema.GroupKind{}, "streaming is not allowed", nil)
		}
		return w, nil
	})
	lw.ListFunc = func(options metav1.ListOptions) (runtime.Object, error) {
		m.Lock()
		defer m.Unlock()
		return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: pods}, nil
	}

	go func() {
		p := makePodFn("p2", "2")
		m.Lock()
		pods = append(pods, *p)
		m.Unlock()
		w.Action(watch.Added, p)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertListCounter(t, lw, 1)
	assertWatchCounter(t, lw, 2)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertStore(t, s, pods)
}

// TestWatchListHappyPath proves that the reflector is synced after receiving a bookmark event
func TestWatchListHappyPath(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent}}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()

	go func() {
		p := makePodFn("p1", "1")
		pods = append(pods, *p)
		w.Action(watch.Added, p)

		p = makePodFn("p2", "2")
		pods = append(pods, *p)
		w.Action(watch.Added, p)

		w.Action(watch.Bookmark, p)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

// TestWatchListWithUpdatesBeforeBookmark checks if Updates and Deletes are propagated during streaming
func TestWatchListWithUpdatesBeforeBookmark(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent}}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()

	go func() {
		p1 := makePodFn("p1", "1")
		w.Action(watch.Added, p1)

		p2 := makePodFn("p2", "2")
		pods = append(pods, *p2)
		w.Action(watch.Added, p2)

		p1.ResourceVersion = "3"
		pods = append(pods, *p1)
		w.Action(watch.Modified, p1)

		p3 := makePodFn("p3", "4")
		w.Action(watch.Added, p3)
		w.Action(watch.Deleted, p3)

		w.Action(watch.Bookmark, p3)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

// TestWatchListInitialTooManyRequests checks if the reflector retries 429
func TestWatchListInitialTooManyRequests(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
	}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		if lw.watchCounter < 3 {
			return nil, apierrors.NewTooManyRequests("busy, check again later", 1)
		}
		return w, nil
	})

	go func() {
		p1 := makePodFn("p1", "1")
		pods = append(pods, *p1)
		w.Action(watch.Added, p1)

		w.Action(watch.Bookmark, p1)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 3)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

// TestWatchListRelistCleanSlate check if stopping a watcher results in creating a new watch-list requests
func TestWatchListRelistCleanSlate(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
	}
	pods := []v1.Pod{}
	w2 := watch.NewFake()
	w1, lw, s, r, stopCh := testData()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		if lw.watchCounter == 1 {
			return w1, nil
		}
		return w2, nil
	})

	wChan := make(chan struct{})
	go func() {
		p1 := makePodFn("p1", "1")
		w1.Action(watch.Added, p1)
		w1.Stop()
		close(wChan)
	}()
	go func() {
		<-wChan
		p2 := makePodFn("p2", "2")
		pods = append(pods, *p2)
		w2.Action(watch.Added, p2)
		w2.Action(watch.Bookmark, p2)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 2)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

// TestWatchListRelist checks behaviour of the reflector during a re-list
func TestWatchListRelist(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "2", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
	}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()

	go func() {
		p := makePodFn("p1", "1")
		pods = append(pods, *p)
		w.Action(watch.Added, p)
		p2 := makePodFn("p2", "2")
		pods = append(pods, *p2)
		w.Action(watch.Added, p2)
		w.Action(watch.Bookmark, p2)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}
	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions[0:1])
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)

	stopCh = make(chan struct{})
	w2 := watch.NewFake()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		return w2, nil
	})
	go func() {
		p3 := makePodFn("p3", "3")
		pods = append(pods, *p3)
		w2.Action(watch.Added, p3)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 2)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

// TestWatchRelistExpired checks behaviour of the reflector during an expired re-list
func TestWatchRelistExpired(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "2", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
	}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		return w, nil
	})

	go func() {
		p := makePodFn("p1", "1")
		pods = append(pods, *p)
		w.Action(watch.Added, p)
		p2 := makePodFn("p2", "2")
		pods = append(pods, *p2)
		w.Action(watch.Added, p2)
		w.Action(watch.Bookmark, p2)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}
	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions[0:1])
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)

	stopCh = make(chan struct{})
	pods = []v1.Pod{}
	w2 := watch.NewFake()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		if lw.watchCounter == 2 {
			return nil, apierrors.NewResourceExpired("rv already expired")
		}
		return w2, nil
	})
	go func() {
		p3 := makePodFn("p3", "3")
		pods = append(pods, *p3)
		w2.Action(watch.Added, p3)
		w2.Action(watch.Bookmark, p3)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}

	assertWatchCounter(t, lw, 3)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)
}

func TestWatchRelistExpiredFallbackToList(t *testing.T) {
	expectedWatchRequestOptions := []metav1.ListOptions{
		{ResourceVersion: "0", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "2", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "", AllowWatchBookmarks: true, ResourceVersionMatch: metav1.ResourceVersionMatchMostRecent},
		{ResourceVersion: "3", AllowWatchBookmarks: true},
	}
	pods := []v1.Pod{}
	w, lw, s, r, stopCh := testData()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		return w, nil
	})

	go func() {
		p := makePodFn("p1", "1")
		pods = append(pods, *p)
		w.Action(watch.Added, p)
		p2 := makePodFn("p2", "2")
		pods = append(pods, *p2)
		w.Action(watch.Added, p2)
		w.Action(watch.Bookmark, p2)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}
	assertWatchCounter(t, lw, 1)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions[0:1])
	assertListCounter(t, lw, 0)
	assertStore(t, s, pods)

	m := sync.Mutex{}
	stopCh = make(chan struct{})
	pods = []v1.Pod{}
	w2 := watch.NewFake()
	lw.WithCustomWatchFunction(func(options metav1.ListOptions) (watch.Interface, error) {
		if lw.watchCounter == 2 {
			return nil, apierrors.NewResourceExpired("rv already expired")
		}
		if lw.watchCounter == 3 {
			return nil, apierrors.NewInvalid(schema.GroupKind{}, "streaming is not allowed", nil)
		}
		return w2, nil
	})
	lw.ListFunc = func(options metav1.ListOptions) (runtime.Object, error) {
		m.Lock()
		defer m.Unlock()
		p3 := makePodFn("p3", "3")
		pods = append(pods, *p3)
		return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "3"}, Items: pods}, nil
	}
	go func() {
		p4 := makePodFn("p4", "4")
		m.Lock()
		pods = append(pods, *p4)
		m.Unlock()
		w2.Action(watch.Added, p4)
		close(stopCh)
	}()
	if err := r.ListAndWatch(stopCh); err != nil {
		t.Fatal(err)
	}
	assertWatchCounter(t, lw, 4)
	assertWatchRequestOptions(t, lw, expectedWatchRequestOptions)
	assertListCounter(t, lw, 1)
	assertStore(t, s, pods)
}

func assertWatchRequestOptions(t *testing.T, lw *enhancedTestLW, expectedWatchRequestOptions []metav1.ListOptions) {
	if len(lw.watchRequestOptions) != len(expectedWatchRequestOptions) {
		t.Fatalf("expected to receive exactly %v WATCH requests, got %v", len(expectedWatchRequestOptions), len(lw.watchRequestOptions))
	}

	for index, expectedWatchOption := range expectedWatchRequestOptions {
		actualWatchRequestOption := lw.watchRequestOptions[index]
		if actualWatchRequestOption.TimeoutSeconds == nil {
			t.Fatalf("setting a timeout value for a WATCH request wasn't specified %#v", actualWatchRequestOption)
		}
		actualWatchRequestOption.TimeoutSeconds = nil
		if actualWatchRequestOption != expectedWatchOption {
			t.Fatalf("expected %#v, got %#v", expectedWatchOption, actualWatchRequestOption)
		}
	}
}

func assertListCounter(t *testing.T, lw *enhancedTestLW, expectedListCounter int) {
	if lw.listCounter != expectedListCounter {
		t.Fatalf("unexpected number of LIST requests, got: %v, expected: %v", lw.listCounter, expectedListCounter)
	}
}

func assertWatchCounter(t *testing.T, lw *enhancedTestLW, expectedWatchCounter int) {
	if lw.watchCounter != expectedWatchCounter {
		t.Fatalf("unexpected number of WATCH requests, got: %v, expected: %v", lw.watchCounter, expectedWatchCounter)
	}
}

func assertStore(t *testing.T, s Store, expectedPods []v1.Pod) {
	rawPods := s.List()
	if len(rawPods) != len(expectedPods) {
		t.Fatalf("expect to find %v items in the storage but found %v", len(expectedPods), len(rawPods))
	}

	actualPods := []v1.Pod{}
	for _, p := range rawPods {
		actualPods = append(actualPods, *p.(*v1.Pod))
	}

	for _, expectedPod := range expectedPods {
		found := false
		for _, actualPod := range actualPods {
			if equality.Semantic.DeepEqual(actualPod, expectedPod) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pod %#v wasn't found in the store", expectedPod)
		}
	}
}

func makePodFn(name, rv string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: rv}}
}

func testData() (*watch.FakeWatcher, *enhancedTestLW, Store, *Reflector, chan struct{}) {
	w := watch.NewFake()
	s := NewStore(MetaNamespaceKeyFunc)
	lw := &enhancedTestLW{
		testLW: &testLW{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return w, nil
			},
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
			},
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)
	r.UseWatchList = true

	return w, lw, s, r, make(chan struct{})
}

type enhancedTestLW struct {
	*testLW
	listCounter  int
	watchCounter int

	watchRequestOptions []metav1.ListOptions

	customWatchFunction func(options metav1.ListOptions) (watch.Interface, error)
}

func (elw *enhancedTestLW) List(options metav1.ListOptions) (runtime.Object, error) {
	elw.listCounter++
	return elw.testLW.ListFunc(options)
}

func (elw *enhancedTestLW) Watch(options metav1.ListOptions) (watch.Interface, error) {
	elw.watchCounter++
	elw.watchRequestOptions = append(elw.watchRequestOptions, options)
	if elw.customWatchFunction != nil {
		return elw.customWatchFunction(options)
	}
	return elw.testLW.WatchFunc(options)
}

func (elw *enhancedTestLW) WithCustomWatchFunction(customFn func(options metav1.ListOptions) (watch.Interface, error)) {
	elw.customWatchFunction = customFn
}
