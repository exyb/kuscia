/*
Copyright 2015 The Kubernetes Authors.

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

package prober

import (
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/probe"

	pkgcontainer "github.com/secretflow/kuscia/pkg/agent/container"
	pkgpod "github.com/secretflow/kuscia/pkg/agent/pod"
	"github.com/secretflow/kuscia/pkg/agent/prober/results"
	"github.com/secretflow/kuscia/pkg/agent/status"
	statustest "github.com/secretflow/kuscia/pkg/agent/status/testing"
)

const (
	testContainerName = "cOnTaInEr_NaMe"
	testPodUID        = "pOd_UiD"
)

var testContainerID = pkgcontainer.CtrID{Type: "test", ID: "cOnTaInEr_Id"}

func getTestRunningStatus() v1.PodStatus {
	return getTestRunningStatusWithStarted(true)
}

func getTestRunningStatusWithStarted(started bool) v1.PodStatus {
	containerStatus := v1.ContainerStatus{
		Name:        testContainerName,
		ContainerID: testContainerID.String(),
	}
	containerStatus.State.Running = &v1.ContainerStateRunning{StartedAt: metav1.Now()}
	containerStatus.Started = &started
	podStatus := v1.PodStatus{
		Phase:             v1.PodRunning,
		ContainerStatuses: []v1.ContainerStatus{containerStatus},
	}
	return podStatus
}

func getTestPod() *v1.Pod {
	container := v1.Container{
		Name: testContainerName,
	}
	pod := v1.Pod{
		Spec: v1.PodSpec{
			Containers:    []v1.Container{container},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	pod.Name = "testPod"
	pod.UID = testPodUID
	return &pod
}

func setTestProbe(pod *v1.Pod, probeType probeType, probeSpec v1.Probe) {
	// All tests rely on the fake tcp prober.
	probeSpec.ProbeHandler = v1.ProbeHandler{
		TCPSocket: &v1.TCPSocketAction{Port: intstr.IntOrString{Type: intstr.Int, IntVal: 80}, Host: "127.0.0.1"},
	}

	// Apply test defaults, overridden for test speed.
	defaults := map[string]int64{
		"TimeoutSeconds":   1,
		"PeriodSeconds":    1,
		"SuccessThreshold": 1,
		"FailureThreshold": 1,
	}
	for field, value := range defaults {
		f := reflect.ValueOf(&probeSpec).Elem().FieldByName(field)
		if f.Int() == 0 {
			f.SetInt(value)
		}
	}

	switch probeType {
	case readiness:
		pod.Spec.Containers[0].ReadinessProbe = &probeSpec
	case liveness:
		pod.Spec.Containers[0].LivenessProbe = &probeSpec
	case startup:
		pod.Spec.Containers[0].StartupProbe = &probeSpec
	}
}

func newTestManager() *manager {
	podManager := pkgpod.NewBasicPodManager(nil)
	// Add test pod to pod manager, so that status manager can get the pod from pod manager if needed.
	podManager.AddPod(getTestPod())
	m := NewManager(
		status.NewManager(&fake.Clientset{}, podManager, &statustest.FakePodDeletionSafetyProvider{}),
		results.NewManager(),
		results.NewManager(),
		results.NewManager(),
		&record.FakeRecorder{},
	).(*manager)
	// Don't actually execute probes.
	return m
}

func newTestWorker(m *manager, probeType probeType, probeSpec v1.Probe) *worker {
	pod := getTestPod()
	setTestProbe(pod, probeType, probeSpec)
	return newWorker(m, probeType, pod, pod.Spec.Containers[0])
}

type fakeTCPProber struct {
	result probe.Result
	err    error
}

func (p fakeTCPProber) Probe(host string, port int, timeout time.Duration) (probe.Result, string, error) {
	return p.result, "", p.err
}

type syncTCPProber struct {
	sync.RWMutex
	fakeTCPProber
}

func (p *syncTCPProber) set(result probe.Result, err error) {
	p.Lock()
	defer p.Unlock()
	p.result = result
	p.err = err
}

func (p *syncTCPProber) Probe(host string, port int, timeout time.Duration) (probe.Result, string, error) {
	p.RLock()
	defer p.RUnlock()
	return p.fakeTCPProber.Probe(host, port, timeout)
}
