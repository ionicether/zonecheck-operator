/*
Copyright 2026 ionicether.

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

package snapshot

import (
	"context"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	checksv1alpha1 "github.com/ionicether/zonecheck-operator/api/v1alpha1"
)

func node(name string, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10"),
				corev1.ResourceMemory: resource.MustParse("100Gi"),
			},
		},
	}
}

func zoned(name, zone string) corev1.Node {
	return node(name, map[string]string{corev1.LabelTopologyZone: zone})
}

func pod(name, nodeName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func owned(p corev1.Pod, apiVersion, kind, name string) corev1.Pod {
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: new(true)}}
	return p
}

func rs(name, nodeName string) corev1.Pod {
	return owned(pod(name, nodeName), "apps/v1", "ReplicaSet", "web")
}

func requesting(p corev1.Pod, cpu, mem string) corev1.Pod {
	p.Spec.Containers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}}}}
	return p
}

func podNames(ps []corev1.Pod) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// outcome maps pod name to "lost: reason", "stuck: reason", or "moves".
func outcome(loss Loss) map[string]string {
	out := map[string]string{}
	for _, l := range loss.Lost {
		out[l.Pod.Name] = "lost: " + l.Reason
	}
	for _, d := range loss.Displaced {
		if d.StuckReason == "" {
			out[d.Pod.Name] = "moves"
		} else {
			out[d.Pod.Name] = "stuck: " + d.StuckReason
		}
	}
	return out
}

func TestTake(t *testing.T) {
	done := pod("done", "a")
	done.Status.Phase = corev1.PodSucceeded
	failed := pod("failed", "a")
	failed.Status.Phase = corev1.PodFailed
	unbound := pod("unbound", "")
	unbound.Status.Phase = corev1.PodPending
	terminating := pod("terminating", "a")
	terminating.DeletionTimestamp = new(metav1.Now())
	terminating.Finalizers = []string{"test"} // fake client refuses a deletionTimestamp without one

	c := fake.NewClientBuilder().WithObjects(
		new(zoned("b", "z1")), new(zoned("a", "z1")),
		new(pod("running", "a")), new(pod("orphan", "gone")),
		&done, &failed, &unbound, &terminating,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch"}},
	).Build()

	s, err := Take(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Nodes[0].Name != "a" || s.Nodes[1].Name != "b" {
		t.Errorf("nodes not sorted: %s, %s", s.Nodes[0].Name, s.Nodes[1].Name)
	}
	if got := podNames(s.Pods); !reflect.DeepEqual(got, []string{"running"}) {
		t.Errorf("pods = %v", got)
	}
	if s.Jobs[types.NamespacedName{Namespace: "default", Name: "batch"}] == nil {
		t.Error("job missing from snapshot")
	}
}

func TestFailures(t *testing.T) {
	s := &Snapshot{
		Nodes: []corev1.Node{
			node("a", map[string]string{corev1.LabelTopologyZone: "z1", "pool": "general"}),
			node("b", map[string]string{corev1.LabelTopologyZone: "z2", "pool": "gpu"}),
			node("c", map[string]string{corev1.LabelTopologyZone: "z1", "pool": "gpu"}),
			node("d", map[string]string{"pool": "general"}),
			node("e", map[string]string{corev1.LabelTopologyZone: ""}),
		},
		Pods: []corev1.Pod{requesting(rs("web", "b"), "1", "1Gi")},
	}

	tests := []struct {
		name string
		sc   checksv1alpha1.Scenario
		want []Failure
	}{
		{
			name: "zone skips unlabeled and empty-labeled nodes",
			sc:   checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioZone},
			want: []Failure{
				{Type: checksv1alpha1.ScenarioZone, Target: "z1", Nodes: []string{"a", "c"}},
				{Type: checksv1alpha1.ScenarioZone, Target: "z2", Nodes: []string{"b"}},
			},
		},
		{
			name: "custom zone label",
			sc:   checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioZone, ZoneLabel: "pool"},
			want: []Failure{
				{Type: checksv1alpha1.ScenarioZone, Target: "general", Nodes: []string{"a", "d"}},
				{Type: checksv1alpha1.ScenarioZone, Target: "gpu", Nodes: []string{"b", "c"}},
			},
		},
		{
			name: "busiest node",
			sc:   checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioBusiestNode},
			want: []Failure{{Type: checksv1alpha1.ScenarioBusiestNode, Target: "b", Nodes: []string{"b"}}},
		},
		{
			name: "node pool",
			sc: checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioNodePool,
				NodePool: &checksv1alpha1.NodePoolRef{LabelKey: "pool", Name: "gpu"}},
			want: []Failure{{Type: checksv1alpha1.ScenarioNodePool, Target: "gpu", Nodes: []string{"b", "c"}}},
		},
		{
			name: "unknown node pool still reports",
			sc: checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioNodePool,
				NodePool: &checksv1alpha1.NodePoolRef{LabelKey: "pool", Name: "typo"}},
			want: []Failure{{Type: checksv1alpha1.ScenarioNodePool, Target: "typo"}},
		},
		{
			name: "node pool without a pool, as when validation is bypassed",
			sc:   checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioNodePool},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.Failures(tt.sc); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}

	if got := s.Unzoned(corev1.LabelTopologyZone); !reflect.DeepEqual(got, []string{"d", "e"}) {
		t.Errorf("unzoned = %v", got)
	}
}

func TestBusiestNode(t *testing.T) {
	static := requesting(pod("static", "c"), "9", "90Gi")
	static.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}

	s := &Snapshot{
		Nodes: []corev1.Node{zoned("a", "z1"), zoned("b", "z1"), zoned("c", "z1"), zoned("d", "z1")},
		Pods: []corev1.Pod{
			// a: 10% of cluster CPU, 2.5% of memory
			requesting(rs("cpu-heavy", "a"), "4", "10Gi"),
			// b: 2.5% of CPU, 15% of memory, so b wins on share
			requesting(rs("mem-heavy", "b"), "1", "60Gi"),
			// these don't need room elsewhere, so they don't count
			static,
			requesting(owned(pod("logs", "c"), "apps/v1", "DaemonSet", "logs"), "9", "90Gi"),
			requesting(pod("bare", "d"), "9", "90Gi"),
		},
	}
	if got := s.busiestNode(); got != "b" {
		t.Errorf("busiest = %q, want b", got)
	}

	idle := &Snapshot{Nodes: []corev1.Node{zoned("a", "z1"), zoned("b", "z1")}}
	if got := idle.busiestNode(); got != "a" {
		t.Errorf("no pods: busiest = %q, want the first node", got)
	}
	if got := (&Snapshot{}).Failures(checksv1alpha1.Scenario{Type: checksv1alpha1.ScenarioBusiestNode}); got != nil {
		t.Errorf("empty cluster: got %+v", got)
	}
}

func TestWithout(t *testing.T) {
	static := pod("static", "a")
	static.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}

	patient := rs("patient", "a")
	patient.Spec.Tolerations = []corev1.Toleration{{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists}}

	tolerateAll := rs("tolerate-all", "a")
	tolerateAll.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}

	// The usual default: tolerate for 300s, then get evicted.
	timed := rs("timed", "a")
	timed.Spec.Tolerations = []corev1.Toleration{{
		Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists,
		Effect: corev1.TaintEffectNoExecute, TolerationSeconds: new(int64(300)),
	}}

	lookalike := owned(pod("lookalike", "a"), "example.com/v1", "DaemonSet", "x")

	s := &Snapshot{
		Nodes: []corev1.Node{zoned("a", "z1"), zoned("b", "z2")},
		Pods: []corev1.Pod{
			rs("web", "a"),
			owned(pod("db", "a"), "apps/v1", "StatefulSet", "db"),
			owned(pod("logs", "a"), "apps/v1", "DaemonSet", "logs"),
			static,
			pod("bare", "a"),
			patient,
			tolerateAll,
			timed,
			lookalike,
			rs("safe", "b"),
		},
	}

	rest, loss := s.Without([]string{"a"}, nil)

	if len(rest.Nodes) != 1 || rest.Nodes[0].Name != "b" {
		t.Errorf("remaining nodes = %v", rest.Nodes)
	}
	if got := podNames(rest.Pods); !reflect.DeepEqual(got, []string{"safe"}) {
		t.Errorf("remaining pods = %v", got)
	}
	want := map[string]string{
		"web":          "moves",
		"db":           "stuck: StatefulSet pods wait for the node or pod to be deleted",
		"bare":         "lost: no controller to recreate it",
		"patient":      "stuck: tolerates node.kubernetes.io/unreachable forever",
		"tolerate-all": "stuck: tolerates node.kubernetes.io/unreachable forever",
		"timed":        "moves",
		"lookalike":    "moves",
	}
	if got := outcome(loss); !reflect.DeepEqual(got, want) {
		t.Errorf("outcome =\n%v\nwant\n%v", got, want)
	}
	for _, d := range loss.Displaced {
		if d.Pod.Spec.NodeName != "" {
			t.Errorf("%s still bound to %q", d.Pod.Name, d.Pod.Spec.NodeName)
		}
	}
	if s.Pods[0].Spec.NodeName != "a" {
		t.Error("original snapshot was modified")
	}
}

func TestWithoutJobs(t *testing.T) {
	job := func(name string, mod func(*batchv1.Job)) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		mod(j)
		return j
	}
	jobPod := func(name, jobName string) corev1.Pod {
		return owned(pod(name, "a"), "batch/v1", "Job", jobName)
	}

	s := &Snapshot{
		Nodes: []corev1.Node{zoned("a", "z1"), zoned("b", "z2")},
		Pods: []corev1.Pod{
			jobPod("plain-1", "plain"),
			jobPod("plain-2", "plain"),
			jobPod("wait-1", "wait"),
			jobPod("policy-1", "policy"),
			jobPod("fragile-1", "fragile"),
			jobPod("fragile-2", "fragile"),
			jobPod("unknown-1", "unknown"),
		},
		Jobs: map[types.NamespacedName]*batchv1.Job{},
	}
	for _, j := range []*batchv1.Job{
		job("plain", func(*batchv1.Job) {}),
		job("wait", func(j *batchv1.Job) { j.Spec.PodReplacementPolicy = new(batchv1.Failed) }),
		job("policy", func(j *batchv1.Job) { j.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{} }),
		job("fragile", func(j *batchv1.Job) {
			j.Spec.BackoffLimit = new(int32(2))
			j.Status.Failed = 1
		}),
	} {
		s.Jobs[types.NamespacedName{Namespace: j.Namespace, Name: j.Name}] = j
	}

	_, loss := s.Without([]string{"a"}, nil)
	want := map[string]string{
		"plain-1":   "moves",
		"plain-2":   "moves",
		"wait-1":    "stuck: Job only replaces pods once they've failed",
		"policy-1":  "stuck: Job only replaces pods once they've failed",
		"fragile-1": "lost: Job fails: evicted pods push it past backoffLimit",
		"fragile-2": "lost: Job fails: evicted pods push it past backoffLimit",
		"unknown-1": "moves",
	}
	if got := outcome(loss); !reflect.DeepEqual(got, want) {
		t.Errorf("outcome =\n%v\nwant\n%v", got, want)
	}
}

func TestWithoutEvictionThrottling(t *testing.T) {
	zone := func(name string, size int) []corev1.Node {
		out := make([]corev1.Node, 0, size)
		for i := range size {
			out = append(out, zoned(name+"-"+string(rune('a'+i)), name))
		}
		return out
	}
	notReady := func(n corev1.Node) corev1.Node {
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
		return n
	}
	small := zone("z1", 4) // z1-a .. z1-d
	other := zone("z2", 4)

	tests := []struct {
		name  string
		nodes []corev1.Node
		lose  []string
		ev    *checksv1alpha1.NodeEviction
		stuck string // substring of the reason, "" for moves
	}{
		{"3 of 4 down stops a small zone", append(small, other...), []string{"z1-a", "z1-b", "z1-c"}, nil, "small zone"},
		{"2 down is never a partial outage", append(small, other...), []string{"z1-a", "z1-b"}, nil, ""},
		{"a whole zone still evicts", append(small, other...), []string{"z1-a", "z1-b", "z1-c", "z1-d"}, nil, ""},
		{"losing every zone stops everything", small, []string{"z1-a", "z1-b", "z1-c", "z1-d"}, nil, "every zone"},
		{"already NotReady nodes count", append([]corev1.Node{notReady(small[3])}, append(small[:3:3], other...)...),
			[]string{"z1-a", "z1-b"}, nil, "small zone"},
		{"larger zones keep evicting, slower", append(small, other...), []string{"z1-a", "z1-b", "z1-c"},
			&checksv1alpha1.NodeEviction{LargeClusterSizeThreshold: new(int32(3))}, ""},
		{"higher threshold", append(small, other...), []string{"z1-a", "z1-b", "z1-c"},
			&checksv1alpha1.NodeEviction{UnhealthyZoneThresholdPercent: new(int32(80))}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Snapshot{Nodes: tt.nodes, Pods: []corev1.Pod{rs("web", tt.lose[0])}}
			_, loss := s.Without(tt.lose, tt.ev)
			if len(loss.Displaced) != 1 {
				t.Fatalf("displaced = %+v", loss.Displaced)
			}
			got := loss.Displaced[0].StuckReason
			if tt.stuck == "" && got != "" || tt.stuck != "" && !strings.Contains(got, tt.stuck) {
				t.Errorf("stuck reason = %q, want %q", got, tt.stuck)
			}
		})
	}
}
