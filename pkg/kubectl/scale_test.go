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

package kubectl

import (
	"errors"
	"fmt"
	"testing"
	"time"

	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/scale"
	testcore "k8s.io/client-go/testing"
	"k8s.io/kubernetes/pkg/apis/batch"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/fake"
	batchclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/batch/internalversion"
	kubectltesting "k8s.io/kubernetes/pkg/kubectl/testing"
)

func TestReplicationControllerScaleRetry(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: schema.GroupVersion{Version: "v1"}.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicationcontrollers", Namespaced: true, Kind: "ReplicationController"},
				{Name: "replicationcontrollers/scale", Namespaced: true, Kind: "Scale", Group: "autoscaling", Version: "v1"},
			},
		},
	}

	pathsResources := map[string]runtime.Object{
		"/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale": &autoscalingv1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: autoscalingv1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo-v1",
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale": {
			"PUT": *kerrors.NewConflict(api.Resource("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "", Resource: "replicationcontrollers"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo-v1"
	namespace := metav1.NamespaceDefault

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	if err != nil {
		t.Errorf("Did not expect an error on update conflict failure, got %v", err)
	}
	preconditions = ScalePrecondition{3, ""}
	scaleFunc = ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err = scaleFunc()
	if err == nil {
		t.Errorf("Expected error on precondition failure")
	}
}

func TestReplicationControllerScaleInvalid(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: schema.GroupVersion{Version: "v1"}.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicationcontrollers", Namespaced: true, Kind: "ReplicationController"},
				{Name: "replicationcontrollers/scale", Namespaced: true, Kind: "Scale", Group: "autoscaling", Version: "v1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale": &autoscalingv1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: autoscalingv1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo-v1",
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: 1},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale": {
			"PUT": *kerrors.NewInvalid(api.Kind("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "", Resource: "replicationcontrollers"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo-v1"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	e, ok := err.(ScaleError)
	if err == nil || !ok || e.FailureType != ScaleUpdateFailure {
		t.Errorf("Expected error on invalid update failure, got %v", err)
	}
}

func TestReplicationControllerScale(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: schema.GroupVersion{Version: "v1"}.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicationcontrollers", Namespaced: true, Kind: "ReplicationController"},
				{Name: "replicationcontrollers/scale", Namespaced: true, Kind: "Scale", Group: "autoscaling", Version: "v1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale": &autoscalingv1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: autoscalingv1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo-v1",
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: 2},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "", Resource: "replicationcontrollers"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo-v1"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error occurred = %v while scaling the resource", err)
	}
	currentReplicas := pathsResources["/api/v1/namespaces/default/replicationcontrollers/foo-v1/scale"].(*autoscalingv1.Scale).Spec.Replicas
	if currentReplicas != int32(count) {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, count)
	}
}

func TestReplicationControllerScaleFailsPreconditions(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: schema.GroupVersion{Version: "v1"}.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicationcontrollers", Namespaced: true, Kind: "ReplicationController"},
				{Name: "replicationcontrollers/scale", Namespaced: true, Kind: "Scale", Group: "autoscaling", Version: "v1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/api/v1/namespaces/default/replicationcontrollers/foo/scale": &autoscalingv1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: autoscalingv1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: autoscalingv1.ScaleSpec{Replicas: 10},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "", Resource: "replicationcontrollers"})
	preconditions := ScalePrecondition{2, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err == nil {
		t.Fatal("expected to get an error but none was returned")
	}
	currentReplicas := pathsResources["/api/v1/namespaces/default/replicationcontrollers/foo/scale"].(*autoscalingv1.Scale).Spec.Replicas
	if currentReplicas != 10 {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, 10)
	}
}

type errorJobs struct {
	batchclient.JobInterface
	conflict bool
	invalid  bool
}

func (c *errorJobs) Update(job *batch.Job) (*batch.Job, error) {
	switch {
	case c.invalid:
		return nil, kerrors.NewInvalid(api.Kind(job.Kind), job.Name, nil)
	case c.conflict:
		return nil, kerrors.NewConflict(api.Resource(job.Kind), job.Name, nil)
	}
	return nil, errors.New("Job update failure")
}

func (c *errorJobs) Get(name string, options metav1.GetOptions) (*batch.Job, error) {
	zero := int32(0)
	return &batch.Job{
		Spec: batch.JobSpec{
			Parallelism: &zero,
		},
	}, nil
}

type errorJobClient struct {
	batchclient.JobsGetter
	conflict bool
	invalid  bool
}

func (c *errorJobClient) Jobs(namespace string) batchclient.JobInterface {
	return &errorJobs{
		JobInterface: c.JobsGetter.Jobs(namespace),
		conflict:     c.conflict,
		invalid:      c.invalid,
	}
}

func TestJobScaleRetry(t *testing.T) {
	fake := &errorJobClient{JobsGetter: fake.NewSimpleClientset().Batch(), conflict: true}
	scaler := ScalerFor(schema.GroupKind{Group: batch.GroupName, Kind: "Job"}, fake, nil, schema.GroupResource{})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass != false {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	if err != nil {
		t.Errorf("Did not expect an error on update failure, got %v", err)
	}
	preconditions = ScalePrecondition{3, ""}
	scaleFunc = ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err = scaleFunc()
	if err == nil {
		t.Error("Expected error on precondition failure")
	}
}

func job() *batch.Job {
	return &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "foo",
		},
	}
}

func TestJobScale(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset(job())
	scaler := ScalerFor(schema.GroupKind{Group: batch.GroupName, Kind: "Job"}, fakeClientset.Batch(), nil, schema.GroupResource{})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	scaler.Scale("default", name, count, &preconditions, nil, nil)

	actions := fakeClientset.Actions()
	if len(actions) != 2 {
		t.Errorf("unexpected actions: %v, expected 2 actions (get, update)", actions)
	}
	if action, ok := actions[0].(testcore.GetAction); !ok || action.GetResource().GroupResource() != batch.Resource("jobs") || action.GetName() != name {
		t.Errorf("unexpected action: %v, expected get-job %s", actions[0], name)
	}
	if action, ok := actions[1].(testcore.UpdateAction); !ok || action.GetResource().GroupResource() != batch.Resource("jobs") || *action.GetObject().(*batch.Job).Spec.Parallelism != int32(count) {
		t.Errorf("unexpected action %v, expected update-job with parallelism = %d", actions[1], count)
	}
}

func TestJobScaleInvalid(t *testing.T) {
	fake := &errorJobClient{JobsGetter: fake.NewSimpleClientset().Batch(), invalid: true}
	scaler := ScalerFor(schema.GroupKind{Group: batch.GroupName, Kind: "Job"}, fake, nil, schema.GroupResource{})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	e, ok := err.(ScaleError)
	if err == nil || !ok || e.FailureType != ScaleUpdateFailure {
		t.Errorf("Expected error on invalid update failure, got %v", err)
	}
}

func TestJobScaleFailsPreconditions(t *testing.T) {
	ten := int32(10)
	fake := fake.NewSimpleClientset(&batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "foo",
		},
		Spec: batch.JobSpec{
			Parallelism: &ten,
		},
	})
	scaler := ScalerFor(schema.GroupKind{Group: batch.GroupName, Kind: "Job"}, fake.Batch(), nil, schema.GroupResource{})
	preconditions := ScalePrecondition{2, ""}
	count := uint(3)
	name := "foo"
	scaler.Scale("default", name, count, &preconditions, nil, nil)

	actions := fake.Actions()
	if len(actions) != 1 {
		t.Errorf("unexpected actions: %v, expected 1 actions (get)", actions)
	}
	if action, ok := actions[0].(testcore.GetAction); !ok || action.GetResource().GroupResource() != batch.Resource("jobs") || action.GetName() != name {
		t.Errorf("unexpected action: %v, expected get-job %s", actions[0], name)
	}
}

func TestValidateJob(t *testing.T) {
	zero, ten, twenty := int32(0), int32(10), int32(20)
	tests := []struct {
		preconditions ScalePrecondition
		job           batch.Job
		expectError   bool
		test          string
	}{
		{
			preconditions: ScalePrecondition{-1, ""},
			expectError:   false,
			test:          "defaults",
		},
		{
			preconditions: ScalePrecondition{-1, ""},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
				Spec: batch.JobSpec{
					Parallelism: &ten,
				},
			},
			expectError: false,
			test:        "defaults 2",
		},
		{
			preconditions: ScalePrecondition{0, ""},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
				Spec: batch.JobSpec{
					Parallelism: &zero,
				},
			},
			expectError: false,
			test:        "size matches",
		},
		{
			preconditions: ScalePrecondition{-1, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
				Spec: batch.JobSpec{
					Parallelism: &ten,
				},
			},
			expectError: false,
			test:        "resource version matches",
		},
		{
			preconditions: ScalePrecondition{10, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
				Spec: batch.JobSpec{
					Parallelism: &ten,
				},
			},
			expectError: false,
			test:        "both match",
		},
		{
			preconditions: ScalePrecondition{10, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
				Spec: batch.JobSpec{
					Parallelism: &twenty,
				},
			},
			expectError: true,
			test:        "size different",
		},
		{
			preconditions: ScalePrecondition{10, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "foo",
				},
			},
			expectError: true,
			test:        "parallelism nil",
		},
		{
			preconditions: ScalePrecondition{10, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "bar",
				},
				Spec: batch.JobSpec{
					Parallelism: &ten,
				},
			},
			expectError: true,
			test:        "version different",
		},
		{
			preconditions: ScalePrecondition{10, "foo"},
			job: batch.Job{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "bar",
				},
				Spec: batch.JobSpec{
					Parallelism: &twenty,
				},
			},
			expectError: true,
			test:        "both different",
		},
	}
	for _, test := range tests {
		err := test.preconditions.ValidateJob(&test.job)
		if err != nil && !test.expectError {
			t.Errorf("unexpected error: %v (%s)", err, test.test)
		}
		if err == nil && test.expectError {
			t.Errorf("expected an error: %v (%s)", err, test.test)
		}
	}
}

func TestDeploymentScaleRetry(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": &appsv1beta2.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta2.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta2.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": {
			"PUT": *kerrors.NewConflict(api.Resource("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "deployments"})
	preconditions := &ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass != false {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	if err != nil {
		t.Errorf("Did not expect an error on update failure, got %v", err)
	}
	preconditions = &ScalePrecondition{3, ""}
	scaleFunc = ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err = scaleFunc()
	if err == nil {
		t.Error("Expected error on precondition failure")
	}
}

func TestDeploymentScale(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": &appsv1beta2.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta2.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta2.ScaleSpec{Replicas: 1},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "deployments"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	currentReplicas := pathsResources["/apis/apps/v1beta2/namespaces/default/deployments/foo/scale"].(*appsv1beta2.Scale).Spec.Replicas
	if currentReplicas != int32(count) {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, count)
	}
}

func TestDeploymentScaleInvalid(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": &appsv1beta2.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta2.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta2.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": {
			"PUT": *kerrors.NewInvalid(api.Kind("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "deployments"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	e, ok := err.(ScaleError)
	if err == nil || !ok || e.FailureType != ScaleUpdateFailure {
		t.Errorf("Expected error on invalid update failure, got %v", err)
	}
}

func TestDeploymentScaleFailsPreconditions(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/foo/scale": &appsv1beta2.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta2.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta2.ScaleSpec{Replicas: 10},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "deployments"})
	preconditions := ScalePrecondition{2, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err == nil {
		t.Fatal("exptected to get an error but none was returned")
	}
	currentReplicas := pathsResources["/apis/apps/v1beta2/namespaces/default/deployments/foo/scale"].(*appsv1beta2.Scale).Spec.Replicas
	if currentReplicas != 10 {
		t.Fatalf("unexpected number of replicas =%v, expected =%v", currentReplicas, 10)
	}
}

func TestStatefulSetScale(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "statefullsets", Namespaced: true, Kind: "StatefullSet"},
				{Name: "statefullsets/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale": &appsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "statefullset"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	currentReplicas := pathsResources["/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale"].(*appsv1beta1.Scale).Spec.Replicas
	if currentReplicas != int32(count) {
		t.Fatalf("unexpected number of replicas =%v, expected =%v", currentReplicas, count)
	}
}

func TestStatefulSetScaleRetry(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "statefullsets", Namespaced: true, Kind: "StatefullSet"},
				{Name: "statefullsets/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale": &appsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale": {
			"PUT": *kerrors.NewConflict(api.Resource("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "statefullset"})
	preconditions := &ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass != false {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	if err != nil {
		t.Errorf("Did not expect an error on update failure, got %v", err)
	}
	preconditions = &ScalePrecondition{3, ""}
	scaleFunc = ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err = scaleFunc()
	if err == nil {
		t.Error("Expected error on precondition failure")
	}
}

func TestStatefulSetScaleInvalid(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "statefull", Namespaced: true, Kind: "StatefullSet"},
				{Name: "statefull/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale": &appsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "statefullset"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	e, ok := err.(ScaleError)
	if err == nil || !ok || e.FailureType != ScaleUpdateFailure {
		t.Errorf("Expected error on invalid update failure, got %v", err)
	}
}

func TestStatefulSetScaleFailsPreconditions(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "statefullsets", Namespaced: true, Kind: "StatefullSet"},
				{Name: "statefullsets/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale": &appsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: appsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: appsv1beta1.ScaleSpec{Replicas: 10},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "apps", Resource: "statefullset"})
	preconditions := ScalePrecondition{2, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err == nil {
		t.Fatal("expected to get an error but none was returned")
	}
	currentReplicas := pathsResources["/apis/apps/v1beta1/namespaces/default/statefullsets/foo/scale"].(*appsv1beta1.Scale).Spec.Replicas
	if currentReplicas != 10 {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, 10)
	}
}

func TestReplicaSetScale(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
				{Name: "replicasets/scale", Namespaced: true, Kind: "Scale", Group: "extensions", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": &extensionsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: extensionsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "extensions", Resource: "replicasets"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	currentReplicas := pathsResources["/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale"].(*extensionsv1beta1.Scale).Spec.Replicas
	if currentReplicas != int32(count) {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, count)
	}
}

func TestReplicaSetScaleRetry(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
				{Name: "replicasets/scale", Namespaced: true, Kind: "Scale", Group: "extensions", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": &extensionsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: extensionsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": {
			"PUT": *kerrors.NewConflict(api.Resource("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "extensions", Resource: "replicasets"})
	preconditions := &ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass != false {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	if err != nil {
		t.Errorf("Did not expect an error on update failure, got %v", err)
	}
	preconditions = &ScalePrecondition{3, ""}
	scaleFunc = ScaleCondition(scaler, preconditions, namespace, name, count, nil)
	pass, err = scaleFunc()
	if err == nil {
		t.Error("Expected error on precondition failure")
	}
}

func TestReplicaSetScaleInvalid(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
				{Name: "replicasets/scale", Namespaced: true, Kind: "Scale", Group: "extensions", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": &extensionsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: extensionsv1beta1.ScaleSpec{Replicas: 2},
		},
	}
	pathsOnError := map[string]map[string]kerrors.StatusError{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": {
			"PUT": *kerrors.NewInvalid(api.Kind("Status"), "foo", nil),
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, pathsOnError)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "extensions", Resource: "replicasets"})
	preconditions := ScalePrecondition{-1, ""}
	count := uint(3)
	name := "foo"
	namespace := "default"

	scaleFunc := ScaleCondition(scaler, &preconditions, namespace, name, count, nil)
	pass, err := scaleFunc()
	if pass {
		t.Errorf("Expected an update failure to return pass = false, got pass = %v", pass)
	}
	e, ok := err.(ScaleError)
	if err == nil || !ok || e.FailureType != ScaleUpdateFailure {
		t.Errorf("Expected error on invalid update failure, got %v", err)
	}
}

func TestReplicaSetsGetterFailsPreconditions(t *testing.T) {
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
				{Name: "replicasets/scale", Namespaced: true, Kind: "Scale", Group: "extensions", Version: "v1beta1"},
			},
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale": &extensionsv1beta1.Scale{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Scale",
				APIVersion: extensionsv1beta1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			},
			Spec: extensionsv1beta1.ScaleSpec{Replicas: 10},
		},
	}
	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	scaler := ScalerFor(schema.GroupKind{}, nil, scaleClient, schema.GroupResource{Group: "extensions", Resource: "replicasets"})
	preconditions := ScalePrecondition{2, ""}
	count := uint(3)
	name := "foo"
	err = scaler.Scale("default", name, count, &preconditions, nil, nil)
	if err == nil {
		t.Fatal("expected to get an error but non was returned")
	}
	currentReplicas := pathsResources["/apis/extensions/v1beta1/namespaces/default/replicasets/foo/scale"].(*extensionsv1beta1.Scale).Spec.Replicas
	if currentReplicas != 10 {
		t.Fatalf("unexpected number of replicas = %v, expected = %v", currentReplicas, 10)
	}
}

// TestGenericScaleSimple exercises GenericScaler.ScaleSimple method
func TestGenericScaleSimple(t *testing.T) {
	// test data
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	appsV1beta2Scale := &appsv1beta2.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: appsv1beta2.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "abc",
		},
		Spec: appsv1beta2.ScaleSpec{Replicas: 10},
		Status: appsv1beta2.ScaleStatus{
			Replicas: 10,
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/abc/scale": appsV1beta2Scale,
	}

	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}

	// test scenarios
	scenarios := []struct {
		name         string
		precondition ScalePrecondition
		newSize      int
		targetGR     schema.GroupResource
		resName      string
		scaleGetter  scale.ScalesGetter
		expectError  bool
	}{
		// scenario 1: scale up the "abc" deployment
		{
			name:         "scale up the \"abc\" deployment",
			precondition: ScalePrecondition{10, ""},
			newSize:      20,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
		},
		// scenario 2: scale down the "abc" deployment
		{
			name:         "scale down the \"abs\" deployment",
			precondition: ScalePrecondition{20, ""},
			newSize:      5,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
		},
		// scenario 3: precondition error, expected size is 1,
		// note that the previous scenario (2) set the size to 5
		{
			name:         "precondition error, expected size is 1",
			precondition: ScalePrecondition{1, ""},
			newSize:      5,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
			expectError:  true,
		},
		// scenario 4: precondition is not validated when the precondition size is set to -1
		{
			name:         "precondition is not validated when the size is set to -1",
			precondition: ScalePrecondition{-1, ""},
			newSize:      5,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
		},
		// scenario 5: precondition error, resource version mismatch
		{
			name:         "precondition error, resource version mismatch",
			precondition: ScalePrecondition{5, "v1"},
			newSize:      5,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
			expectError:  true,
		},
	}

	// act
	for index, scenario := range scenarios {
		t.Run(fmt.Sprintf("running scenario %d: %s", index+1, scenario.name), func(t *testing.T) {
			target := ScalerFor(schema.GroupKind{}, nil, scenario.scaleGetter, scenario.targetGR)

			resVersion, err := target.ScaleSimple("default", scenario.resName, &scenario.precondition, uint(scenario.newSize))

			if scenario.expectError && err == nil {
				t.Fatal("expeced an error but was not returned")
			}
			if !scenario.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resVersion != "" {
				t.Fatalf("unexpected resource version returned = %s, wanted = %s", resVersion, "")
			}
		})
	}
}

// TestGenericScale exercises GenericScaler.Scale method
func TestGenericScale(t *testing.T) {
	// test data
	discoveryResources := []*metav1.APIResourceList{
		{
			GroupVersion: appsv1beta2.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Group: "apps", Version: "v1beta2"},
			},
		},
	}
	appsV1beta2Scale := &appsv1beta2.Scale{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Scale",
			APIVersion: appsv1beta2.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "abc",
		},
		Spec: appsv1beta2.ScaleSpec{Replicas: 10},
		Status: appsv1beta2.ScaleStatus{
			Replicas: 10,
		},
	}
	pathsResources := map[string]runtime.Object{
		"/apis/apps/v1beta2/namespaces/default/deployments/abc/scale": appsV1beta2Scale,
	}

	scaleClient, err := kubectltesting.FakeScaleClient(discoveryResources, pathsResources, nil)
	if err != nil {
		t.Fatal(err)
	}

	// test scenarios
	scenarios := []struct {
		name            string
		precondition    ScalePrecondition
		newSize         int
		targetGR        schema.GroupResource
		resName         string
		scaleGetter     scale.ScalesGetter
		waitForReplicas *RetryParams
		expectError     bool
	}{
		// scenario 1: scale up the "abc" deployment
		{
			name:         "scale up the \"abc\" deployment",
			precondition: ScalePrecondition{10, ""},
			newSize:      20,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "abc",
			scaleGetter:  scaleClient,
		},
		// scenario 2: a resource name cannot be empty
		{
			name:         "a resource name cannot be empty",
			precondition: ScalePrecondition{10, ""},
			newSize:      20,
			targetGR:     schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:      "",
			scaleGetter:  scaleClient,
			expectError:  true,
		},
		// scenario 3: wait for replicas error due to status.Replicas != spec.Replicas
		{
			name:            "wait for replicas error due to status.Replicas != spec.Replicas",
			precondition:    ScalePrecondition{10, ""},
			newSize:         20,
			targetGR:        schema.GroupResource{Group: "apps", Resource: "deployment"},
			resName:         "abc",
			scaleGetter:     scaleClient,
			waitForReplicas: &RetryParams{time.Duration(5 * time.Second), time.Duration(5 * time.Second)},
			expectError:     true,
		},
	}

	// act
	for index, scenario := range scenarios {
		t.Run(fmt.Sprintf("running scenario %d: %s", index+1, scenario.name), func(t *testing.T) {
			target := ScalerFor(schema.GroupKind{}, nil, scenario.scaleGetter, scenario.targetGR)

			err := target.Scale("default", scenario.resName, uint(scenario.newSize), &scenario.precondition, nil, scenario.waitForReplicas)

			if scenario.expectError && err == nil {
				t.Fatal("expeced an error but was not returned")
			}
			if !scenario.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
