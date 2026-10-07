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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	checksv1alpha1 "github.com/ionicether/zonecheck-operator/api/v1alpha1"
	"github.com/ionicether/zonecheck-operator/internal/snapshot"
)

// Match the MaxItems markers on ZoneCheckStatus.Results and ScenarioResult.Pods.
const (
	maxResults    = 64
	maxListedPods = 25
)

// ZoneCheckReconciler runs the simulations for each ZoneCheck.
type ZoneCheckReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=checks.zonecheck.dev,resources=zonechecks,verbs=get;list;watch
// +kubebuilder:rbac:groups=checks.zonecheck.dev,resources=zonechecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch

func (r *ZoneCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var zc checksv1alpha1.ZoneCheck
	if err := r.Get(ctx, req.NamespacedName, &zc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The CRD defaults it, but objects built in Go skip that.
	interval := 5 * time.Minute
	if zc.Spec.Interval != nil {
		interval = zc.Spec.Interval.Duration
	}

	// Don't run early after a restart or resync.
	if zc.Status.ObservedGeneration == zc.Generation && zc.Status.LastRunTime != nil {
		if wait := time.Until(zc.Status.LastRunTime.Add(interval)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	base := zc.DeepCopy()
	snap, err := snapshot.Take(ctx, r.Client)
	if err != nil {
		meta.SetStatusCondition(&zc.Status.Conditions, metav1.Condition{
			Type: checksv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: "SnapshotFailed", Message: err.Error(), ObservedGeneration: zc.Generation,
		})
		if perr := r.Status().Patch(ctx, &zc, client.MergeFrom(base)); perr != nil {
			log.Error(perr, "Failed to record snapshot failure")
		}
		return ctrl.Result{}, err
	}

	zc.Status.Results = simulate(snap, &zc.Spec)
	if len(zc.Status.Results) > maxResults {
		log.Info("Dropped results past the status limit", "results", len(zc.Status.Results), "limit", maxResults)
		zc.Status.Results = zc.Status.Results[:maxResults]
	}
	setZoneLabelCondition(&zc, snap)
	meta.SetStatusCondition(&zc.Status.Conditions, metav1.Condition{
		Type: checksv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "RunSucceeded", ObservedGeneration: zc.Generation,
	})
	zc.Status.ObservedGeneration = zc.Generation
	zc.Status.LastRunTime = &metav1.Time{Time: time.Now()}
	if err := r.Status().Patch(ctx, &zc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("Finished run", "nodes", len(snap.Nodes), "pods", len(snap.Pods), "results", len(zc.Status.Results))
	return ctrl.Result{RequeueAfter: interval}, nil
}

// TODO: place displaced pods on the remaining nodes.
func simulate(snap *snapshot.Snapshot, spec *checksv1alpha1.ZoneCheckSpec) []checksv1alpha1.ScenarioResult {
	var results []checksv1alpha1.ScenarioResult
	for _, sc := range spec.Scenarios {
		for _, f := range snap.Failures(sc) {
			_, loss := snap.Without(f.Nodes, spec.NodeEviction)

			res := checksv1alpha1.ScenarioResult{
				Type:          f.Type,
				Target:        f.Target,
				LostNodes:     int32(len(f.Nodes)),
				DisplacedPods: int32(len(loss.Displaced)),
				LostPods:      int32(len(loss.Lost)),
			}
			for _, l := range loss.Lost {
				addPod(&res, &l.Pod, checksv1alpha1.PodLost, l.Reason)
			}
			for _, d := range loss.Displaced {
				if d.StuckReason != "" {
					res.OutageStuckPods++
					addPod(&res, &d.Pod, checksv1alpha1.PodOutageStuck, d.StuckReason)
				}
			}
			results = append(results, res)
		}
	}
	return results
}

func addPod(res *checksv1alpha1.ScenarioResult, p *corev1.Pod, issue checksv1alpha1.PodIssue, reason string) {
	if len(res.Pods) >= maxListedPods {
		res.OmittedPods++
		return
	}
	ap := checksv1alpha1.AffectedPod{Namespace: p.Namespace, Name: p.Name, Issue: issue, Reason: reason}
	if owner := metav1.GetControllerOf(p); owner != nil {
		ap.Owner = owner.Kind + "/" + owner.Name
	}
	res.Pods = append(res.Pods, ap)
}

func setZoneLabelCondition(zc *checksv1alpha1.ZoneCheck, snap *snapshot.Snapshot) {
	var missing []string
	anyZone := false
	for _, sc := range zc.Spec.Scenarios {
		if sc.Type != checksv1alpha1.ScenarioZone {
			continue
		}
		anyZone = true
		if unzoned := snap.Unzoned(sc.ZoneLabelOrDefault()); len(unzoned) > 0 {
			missing = append(missing, fmt.Sprintf("%d nodes have no %s label (%s)",
				len(unzoned), sc.ZoneLabelOrDefault(), strings.Join(unzoned[:min(len(unzoned), 10)], ", ")))
		}
	}

	switch {
	case !anyZone:
		meta.RemoveStatusCondition(&zc.Status.Conditions, checksv1alpha1.ConditionZoneLabelsComplete)
	case len(missing) > 0:
		meta.SetStatusCondition(&zc.Status.Conditions, metav1.Condition{
			Type: checksv1alpha1.ConditionZoneLabelsComplete, Status: metav1.ConditionFalse,
			Reason: "NodesMissingZoneLabel", Message: strings.Join(missing, "; "), ObservedGeneration: zc.Generation,
		})
	default:
		meta.SetStatusCondition(&zc.Status.Conditions, metav1.Condition{
			Type: checksv1alpha1.ConditionZoneLabelsComplete, Status: metav1.ConditionTrue,
			Reason: "AllNodesZoned", ObservedGeneration: zc.Generation,
		})
	}
}

func (r *ZoneCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Start these informers with the manager so the first run doesn't wait
	// on them, and missing RBAC shows up at startup.
	for _, obj := range []client.Object{&corev1.Node{}, &corev1.Pod{}, &batchv1.Job{}} {
		if _, err := mgr.GetCache().GetInformer(context.Background(), obj); err != nil {
			return err
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Skip status-only updates, or every status write would start a new run.
		For(&checksv1alpha1.ZoneCheck{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("zonecheck").
		Complete(r)
}
