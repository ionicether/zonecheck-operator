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

// Package snapshot holds a point-in-time copy of the nodes and pods a
// simulation runs against.
package snapshot

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-helpers/node/topology"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	checksv1alpha1 "github.com/ionicether/zonecheck-operator/api/v1alpha1"
)

// Snapshot is sorted by name so simulations are repeatable.
type Snapshot struct {
	Nodes []corev1.Node

	// Pods are bound to a node in Nodes and still running or starting.
	// Unbound, finished, and terminating pods are left out.
	Pods []corev1.Pod

	Jobs map[types.NamespacedName]*batchv1.Job
}

// Failure is one concrete thing to simulate losing, like "zone a".
type Failure struct {
	Type   checksv1alpha1.ScenarioType
	Target string
	Nodes  []string
}

// Displaced is a pod a controller will recreate after a drain.
type Displaced struct {
	Pod corev1.Pod

	// StuckReason says why an outage would leave the pod on the dead node.
	// Empty means it moves in an outage too.
	StuckReason string
}

// Lost is a pod nothing will recreate.
type Lost struct {
	Pod    corev1.Pod
	Reason string
}

// Loss is what losing some nodes does to the pods on them.
type Loss struct {
	Displaced []Displaced
	Lost      []Lost
}

func Take(ctx context.Context, c client.Reader) (*Snapshot, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs); err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}

	s := &Snapshot{Nodes: nodes.Items, Jobs: map[types.NamespacedName]*batchv1.Job{}}
	known := make(map[string]bool, len(s.Nodes))
	for _, n := range s.Nodes {
		known[n.Name] = true
	}
	for _, p := range pods.Items {
		// A terminating pod's controller has usually made its replacement
		// already, so counting it too would count the workload twice.
		if !known[p.Spec.NodeName] || p.DeletionTimestamp != nil ||
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		s.Pods = append(s.Pods, p)
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		s.Jobs[types.NamespacedName{Namespace: j.Namespace, Name: j.Name}] = j
	}

	slices.SortFunc(s.Nodes, func(a, b corev1.Node) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(s.Pods, func(a, b corev1.Pod) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return s, nil
}

// Failures expands a scenario into concrete failures, one per zone for Zone.
// NodePool always gives one, even with no matching nodes, so a typo in the
// pool name shows up as 0 lost nodes.
func (s *Snapshot) Failures(sc checksv1alpha1.Scenario) []Failure {
	switch sc.Type {
	case checksv1alpha1.ScenarioZone:
		label := sc.ZoneLabelOrDefault()
		byZone := map[string][]string{}
		for _, n := range s.Nodes {
			if zone := n.Labels[label]; zone != "" {
				byZone[zone] = append(byZone[zone], n.Name)
			}
		}
		out := make([]Failure, 0, len(byZone))
		for _, z := range slices.Sorted(maps.Keys(byZone)) {
			out = append(out, Failure{Type: sc.Type, Target: z, Nodes: byZone[z]})
		}
		return out

	case checksv1alpha1.ScenarioBusiestNode:
		if n := s.busiestNode(); n != "" {
			return []Failure{{Type: sc.Type, Target: n, Nodes: []string{n}}}
		}
		return nil

	case checksv1alpha1.ScenarioNodePool:
		if sc.NodePool == nil {
			return nil
		}
		f := Failure{Type: sc.Type, Target: sc.NodePool.Name}
		for _, n := range s.Nodes {
			if n.Labels[sc.NodePool.LabelKey] == sc.NodePool.Name {
				f.Nodes = append(f.Nodes, n.Name)
			}
		}
		return []Failure{f}
	}
	return nil
}

// Unzoned returns nodes with no value for the zone label. Zone failures
// skip them, so they count as surviving capacity.
func (s *Snapshot) Unzoned(label string) []string {
	var out []string
	for _, n := range s.Nodes {
		if n.Labels[label] == "" {
			out = append(out, n.Name)
		}
	}
	return out
}

// busiestNode picks the node whose movable pods take the biggest share of
// the cluster's CPU or memory. Ties go to the first name.
func (s *Snapshot) busiestNode() string {
	var totalCPU, totalMem int64
	for _, n := range s.Nodes {
		totalCPU += n.Status.Allocatable.Cpu().MilliValue()
		totalMem += n.Status.Allocatable.Memory().Value()
	}

	// Matches kube-scheduler's accounting for running pods in 1.37, where
	// in-place resize and pod-level resources are on by default.
	opts := resourcehelper.PodResourcesOptions{UseStatusResources: true, InPlacePodLevelResourcesVerticalScalingEnabled: true}
	cpu, mem := map[string]int64{}, map[string]int64{}
	for i := range s.Pods {
		p := &s.Pods[i]
		if goesWithNode(p) || metav1.GetControllerOf(p) == nil {
			continue
		}
		req := resourcehelper.PodRequests(p, opts)
		cpu[p.Spec.NodeName] += req.Cpu().MilliValue()
		mem[p.Spec.NodeName] += req.Memory().Value()
	}

	best, bestShare := "", -1.0
	for _, n := range s.Nodes {
		share := max(fraction(cpu[n.Name], totalCPU), fraction(mem[n.Name], totalMem))
		if share > bestShare {
			best, bestShare = n.Name, share
		}
	}
	return best
}

func fraction(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// Without returns what's left after losing nodes, and what happens to their
// pods. Displaced pods are copies with spec.nodeName cleared. The returned
// snapshot shares objects with s, so don't modify it.
func (s *Snapshot) Without(nodes []string, ev *checksv1alpha1.NodeEviction) (*Snapshot, Loss) {
	gone := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		gone[n] = true
	}

	rest := &Snapshot{Jobs: s.Jobs}
	zoneOf := make(map[string]string, len(s.Nodes))
	for i := range s.Nodes {
		n := &s.Nodes[i]
		zoneOf[n.Name] = topology.GetZoneKey(n)
		if !gone[n.Name] {
			rest.Nodes = append(rest.Nodes, *n)
		}
	}
	stopped := s.evictionStopped(gone, ev)

	// A Job fails once its failures pass backoffLimit, and evicted pods
	// count as failures, so tally them per Job first.
	evictedFromJob := map[types.NamespacedName]int32{}
	for i := range s.Pods {
		p := &s.Pods[i]
		if job := s.jobOf(p); gone[p.Spec.NodeName] && job != nil {
			evictedFromJob[client.ObjectKeyFromObject(job)]++
		}
	}

	var loss Loss
	for i := range s.Pods {
		p := &s.Pods[i]
		if !gone[p.Spec.NodeName] {
			rest.Pods = append(rest.Pods, *p)
			continue
		}
		if goesWithNode(p) {
			continue
		}
		if metav1.GetControllerOf(p) == nil {
			loss.Lost = append(loss.Lost, Lost{Pod: *p, Reason: "no controller to recreate it"})
			continue
		}
		job := s.jobOf(p)
		if job != nil && passesBackoffLimit(job, evictedFromJob[client.ObjectKeyFromObject(job)]) {
			loss.Lost = append(loss.Lost, Lost{Pod: *p, Reason: "Job fails: evicted pods push it past backoffLimit"})
			continue
		}
		moved := p.DeepCopy()
		moved.Spec.NodeName = ""
		loss.Displaced = append(loss.Displaced, Displaced{
			Pod:         *moved,
			StuckReason: cmp.Or(stuckReason(p, job), stopped[zoneOf[p.Spec.NodeName]]),
		})
	}
	return rest, loss
}

// evictionStopped follows the node lifecycle controller: a zone where more
// than 2 nodes and at least the threshold share are down is partially
// disrupted, and a small one stops evicting. If every zone is fully down,
// nothing gets evicted anywhere. Nodes already NotReady count as down.
func (s *Snapshot) evictionStopped(gone map[string]bool, ev *checksv1alpha1.NodeEviction) map[string]string {
	type zone struct{ total, down int32 }
	zones := map[string]*zone{}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		k := topology.GetZoneKey(n)
		if zones[k] == nil {
			zones[k] = &zone{}
		}
		zones[k].total++
		if gone[n.Name] || !ready(n) {
			zones[k].down++
		}
	}

	stopped := map[string]string{}
	allDown := len(zones) > 0
	for k, z := range zones {
		if z.down < z.total {
			allDown = false
		}
		if z.down > 2 && z.down < z.total && z.down*100 >= ev.ThresholdPercent()*z.total && z.total <= ev.LargeClusterSize() {
			stopped[k] = "node controller stops evicting in a small zone that's mostly down"
		}
	}
	if allDown {
		for k := range zones {
			stopped[k] = "node controller stops evicting when every zone is down"
		}
	}
	return stopped
}

func ready(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// goesWithNode: DaemonSet and static pods die with their node.
func goesWithNode(p *corev1.Pod) bool {
	if _, ok := p.Annotations[corev1.MirrorPodAnnotationKey]; ok {
		return true
	}
	return ownedBy(p, "apps/v1", "DaemonSet")
}

var unreachable = corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute}

func stuckReason(p *corev1.Pod, job *batchv1.Job) string {
	if ownedBy(p, "apps/v1", "StatefulSet") {
		return "StatefulSet pods wait for the node or pod to be deleted"
	}
	if job != nil && (job.Spec.PodFailurePolicy != nil ||
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed) {
		return "Job only replaces pods once they've failed"
	}
	for i := range p.Spec.Tolerations {
		t := &p.Spec.Tolerations[i]
		if t.TolerationSeconds == nil && t.ToleratesTaint(klog.Background(), &unreachable, false) {
			return "tolerates node.kubernetes.io/unreachable forever"
		}
	}
	return ""
}

// passesBackoffLimit skips Jobs with a pod failure policy (its rules can
// ignore evictions) or per-index limits.
func passesBackoffLimit(job *batchv1.Job, evicted int32) bool {
	if job.Spec.PodFailurePolicy != nil || job.Spec.BackoffLimitPerIndex != nil {
		return false
	}
	limit := int32(6)
	if job.Spec.BackoffLimit != nil {
		limit = *job.Spec.BackoffLimit
	}
	return job.Status.Failed+evicted > limit
}

func (s *Snapshot) jobOf(p *corev1.Pod) *batchv1.Job {
	owner := metav1.GetControllerOf(p)
	if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" {
		return nil
	}
	return s.Jobs[types.NamespacedName{Namespace: p.Namespace, Name: owner.Name}]
}

func ownedBy(p *corev1.Pod, apiVersion, kind string) bool {
	owner := metav1.GetControllerOf(p)
	return owner != nil && owner.APIVersion == apiVersion && owner.Kind == kind
}
