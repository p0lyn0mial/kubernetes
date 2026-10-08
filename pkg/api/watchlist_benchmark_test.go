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

package api_test

import (
	"compress/gzip"
	"fmt"
	"io"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/applyconfigurations"
	"k8s.io/utils/ptr"

	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	coreinstall "k8s.io/kubernetes/pkg/apis/core/install"
)

const watchListBenchmarkEventCount = 150_000

// BenchmarkWatchListProtobufEncoding measures the encoding portion of the
// initial watch-list response. Objects are supplied directly, keeping storage
// and storage decoding out of the measured section.
//
// Run with:
//
//	go test ./pkg/api -run '^$' -bench '^BenchmarkWatchListProtobufEncoding$' -benchtime=1x
func BenchmarkWatchListProtobufEncoding(b *testing.B) {
	coreinstall.Install(legacyscheme.Scheme)
	info, ok := runtime.SerializerInfoForMediaType(legacyscheme.Codecs.SupportedMediaTypes(), runtime.ContentTypeProtobuf)
	if !ok || info.StreamSerializer == nil {
		b.Fatal("protobuf stream serializer is not available")
	}

	externalPod := watchListBenchmarkPod()
	legacyscheme.Scheme.Default(externalPod)
	pod := &api.Pod{}
	if err := legacyscheme.Scheme.Convert(externalPod, pod, nil); err != nil {
		b.Fatal(err)
	}
	fieldManager, err := managedfields.NewDefaultFieldManager(
		applyconfigurations.NewTypeConverter(legacyscheme.Scheme),
		runtime.UnsafeObjectConvertor(legacyscheme.Scheme),
		legacyscheme.Scheme,
		legacyscheme.Scheme,
		corev1.SchemeGroupVersion.WithKind("Pod"),
		api.SchemeGroupVersion,
		"",
		nil,
	)
	if err != nil {
		b.Fatal(err)
	}
	managedPod, err := fieldManager.Update(&api.Pod{}, pod, "clusterloader")
	if err != nil {
		b.Fatal(err)
	}
	pod = managedPod.(*api.Pod)
	storedPod := &corev1.Pod{}
	if err := legacyscheme.Scheme.Convert(pod, storedPod, nil); err != nil {
		b.Fatal(err)
	}
	addWatchListBenchmarkAdmissionFields(storedPod)
	if err := legacyscheme.Scheme.Convert(storedPod, pod, nil); err != nil {
		b.Fatal(err)
	}
	pods := make([]*api.Pod, watchListBenchmarkEventCount)
	for i := range pods {
		pods[i] = pod.DeepCopy()
		pods[i].Name = fmt.Sprintf("list-pod-%d", i)
		pods[i].UID = types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		pods[i].ResourceVersion = fmt.Sprintf("%d", i+1)
	}

	for _, tc := range []struct {
		name       string
		clients    int
		compressed bool
	}{
		{name: "uncompressed/1-client", clients: 1},
		{name: "uncompressed/10-clients", clients: 10},
		{name: "gzip/1-client", clients: 1, compressed: true},
		{name: "gzip/10-clients", clients: 10, compressed: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var bytesWritten int64
			for range b.N {
				b.StopTimer()
				results := make([]int64, tc.clients)
				b.StartTimer()

				var wg sync.WaitGroup
				wg.Add(tc.clients)
				for i := range tc.clients {
					go func() {
						defer wg.Done()
						results[i] = encodeWatchList(b, info, pods, tc.compressed)
					}()
				}
				wg.Wait()
				b.StopTimer()

				bytesWritten = 0
				for _, result := range results {
					bytesWritten += result
				}
			}

			b.ReportMetric(float64(watchListBenchmarkEventCount*tc.clients)*float64(b.N)/b.Elapsed().Seconds(), "events/s")
			b.ReportMetric(float64(bytesWritten)/(1024*1024), "wire-MiB/op")
		})
	}
}

func encodeWatchList(b *testing.B, info runtime.SerializerInfo, pods []*api.Pod, compressed bool) int64 {
	b.Helper()
	allocator := runtime.AllocatorPool.Get().(*runtime.Allocator)
	defer runtime.AllocatorPool.Put(allocator)

	embeddedEncoder := runtime.NewEncoderWithAllocator(
		legacyscheme.Codecs.EncoderForVersion(info.Serializer, corev1.SchemeGroupVersion).(runtime.EncoderWithAllocator),
		allocator,
	)
	eventEncoder := runtime.NewEncoderWithAllocator(
		legacyscheme.Codecs.EncoderForVersion(info.StreamSerializer.Serializer, corev1.SchemeGroupVersion).(runtime.EncoderWithAllocator),
		allocator,
	)

	counter := &countingWriter{delegate: io.Discard}
	var output io.WriteCloser = nopWriteCloser{counter}
	if compressed {
		gzipWriter, err := gzip.NewWriterLevel(counter, gzip.BestSpeed)
		if err != nil {
			b.Error(err)
			return 0
		}
		output = gzipWriter
	}
	framer := info.StreamSerializer.Framer.NewFrameWriter(output)
	embeddedBuffer := runtime.NewSpliceBuffer()
	eventBuffer := runtime.NewSpliceBuffer()

	for i := range watchListBenchmarkEventCount {
		embeddedBuffer.Reset()
		if err := embeddedEncoder.Encode(pods[i%len(pods)], embeddedBuffer); err != nil {
			b.Error(err)
			return 0
		}
		event := &metav1.WatchEvent{
			Type:   string(watch.Added),
			Object: runtime.RawExtension{Raw: embeddedBuffer.Bytes()},
		}
		eventBuffer.Reset()
		if err := eventEncoder.Encode(event, eventBuffer); err != nil {
			b.Error(err)
			return 0
		}
		if _, err := framer.Write(eventBuffer.Bytes()); err != nil {
			b.Error(err)
			return 0
		}
	}
	if err := output.Close(); err != nil {
		b.Error(err)
		return 0
	}
	return counter.written
}

type countingWriter struct {
	delegate io.Writer
	written  int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.delegate.Write(p)
	w.written += int64(n)
	return n, err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// watchListBenchmarkPod is the pod template used by perf-tests' list benchmark,
// with the ClusterLoader template parameters resolved. Keep this in sync with:
// https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/testing/list/pod.yaml
func watchListBenchmarkPod() *corev1.Pod {
	podName := "list-pod-0"
	imageRegistry := "registry.k8s.io"
	env := func(containerName string) []corev1.EnvVar {
		return []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
			{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "GOMAXPROCS", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: containerName, Resource: "limits.cpu"}}},
			{Name: "GOMEMLIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: containerName, Resource: "limits.memory"}}},
			{Name: "JAVA_TOOL_OPTIONS", Value: "-XX:MaxRAMPercentage=75.0"},
			{Name: "REDIS_URL", Value: "redis://main:6379/0"},
			{Name: "PYTHONUNBUFFERED", Value: "1"},
			{Name: "NODE_ENV", Value: "prod"},
		}
	}
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			GenerateName:    "list-pod-",
			Namespace:       "test-namespace",
			UID:             types.UID("00000000-0000-0000-0000-000000000000"),
			ResourceVersion: "1",
			Labels: map[string]string{
				"group":                       "list-pod",
				"app.kubernetes.io/name":      "payment-service",
				"app.kubernetes.io/instance":  "payment-service-primary",
				"app.kubernetes.io/version":   "v2.1.4",
				"app.kubernetes.io/component": "backend",
				"app.kubernetes.io/part-of":   "ecommerce-platform",
				"env":                         "production",
				"track":                       "canary",
			},
			Annotations: map[string]string{
				"prometheus.io/scrape":                           "false",
				"prometheus.io/port":                             "8080",
				"prometheus.io/path":                             "/metrics",
				"sidecar.istio.io/inject":                        "false",
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
				"vault.hashicorp.com/agent-inject":               "false",
				"vault.hashicorp.com/role":                       "my-app-db-role",
				"fluentbit.io/parser":                            "json",
				"argocd.argoproj.io/hook":                        "PreSync",
				"argocd.argoproj.io/hook-delete-policy":          "HookSucceeded",
			},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "do-not-schedule",
			InitContainers: []corev1.Container{
				{Name: "init-0", Image: imageRegistry + "/e2e-test-images/agnhost:2.53", Command: []string{"/bin/sh", "-c", "sleep 1"}},
				{Name: "init-1", Image: imageRegistry + "/e2e-test-images/agnhost:2.53", Command: []string{"/bin/sh", "-c", "sleep 1"}},
			},
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: imageRegistry + "/pause:3.9",
					Env:   env("main"),
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("5m"),
						corev1.ResourceMemory: resource.MustParse("20M"),
					}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "configmap", MountPath: "/var/configmap"},
						{Name: "secret", MountPath: "/var/secret"},
					},
				},
				{
					Name:  "sidecar",
					Image: imageRegistry + "/pause:3.9",
					Env:   env("sidecar"),
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("5m"),
						corev1.ResourceMemory: resource.MustParse("20M"),
					}},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "configmap", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: podName}, Optional: ptr.To(true)}}},
				{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: podName, Optional: ptr.To(true)}}},
			},
		},
	}
}

// These fields are added after field ownership is calculated, as they are by
// admission and controllers in the perf test. They account for the remaining
// difference between the submitted template and the stored pod being encoded.
func addWatchListBenchmarkAdmissionFields(pod *corev1.Pod) {
	const serviceAccountVolume = "kube-api-access-abcde"
	pod.Spec.ServiceAccountName = "default"
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: serviceAccountVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](420),
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: ptr.To[int64](3607), Path: "token"}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
			},
		}},
	})
	mount := corev1.VolumeMount{Name: serviceAccountVolume, ReadOnly: true, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, mount)
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mount)
	}
	pod.Spec.Tolerations = append(pod.Spec.Tolerations,
		corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](300)},
		corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](300)},
	)
	pod.Spec.Priority = ptr.To[int32](0)
	pod.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
	pod.Status.Phase = corev1.PodPending
	pod.Status.QOSClass = corev1.PodQOSBurstable
}
