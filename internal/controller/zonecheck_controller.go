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
	"github.com/ionicether/zonecheck-operator/internal/placement"
	"github.com/ionicether/zonecheck-operator/internal/snapshot"
)

// keep in sync w/ the MaxItems/MaxLength markers in the API
const (
	maxResults    = 64
	maxListedPods = 25
	maxReasonLen  = 512
)

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
	// the CRD defaults it, but objects built in Go don't get that
	interval := 5 * time.Minute
	if zc.Spec.Interval != nil {
		interval = zc.Spec.Interval.Duration
	}

	// don't run early after a restart/resync
	if zc.Status.ObservedGeneration == zc.Generation && zc.Status.LastRunTime != nil {
		if wait := time.Until(zc.Status.LastRunTime.Add(interval)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	base := zc.DeepCopy()
	fail := func(reason string, err error) (ctrl.Result, error) {
		meta.SetStatusCondition(&zc.Status.Conditions, metav1.Condition{
			Type: checksv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: reason, Message: err.Error(), ObservedGeneration: zc.Generation,
		})
		if perr := r.Status().Patch(ctx, &zc, client.MergeFrom(base)); perr != nil {
			log.Error(perr, "Failed to record failed run", "reason", reason)
		}
		return ctrl.Result{}, err
	}

	snap, err := snapshot.Take(ctx, r.Client)
	if err != nil {
		return fail("SnapshotFailed", err)
	}
	results, err := simulate(ctx, snap, &zc.Spec)
	if err != nil {
		return fail("SimulationFailed", err)
	}
	zc.Status.Results = results
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

func simulate(ctx context.Context, snap *snapshot.Snapshot, spec *checksv1alpha1.ZoneCheckSpec) ([]checksv1alpha1.ScenarioResult, error) {
	var results []checksv1alpha1.ScenarioResult
	for _, sc := range spec.Scenarios {
		for _, f := range snap.Failures(sc) {
			rest, loss := snap.Without(f.Nodes, spec.NodeEviction)

			displaced := make([]corev1.Pod, len(loss.Displaced))
			for i, d := range loss.Displaced {
				displaced[i] = d.Pod
			}
			placed, err := placement.Place(ctx, rest.Nodes, rest.Pods, displaced)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", f.Type, f.Target, err)
			}

			res := checksv1alpha1.ScenarioResult{
				Type:          f.Type,
				Target:        f.Target,
				LostNodes:     int32(len(f.Nodes)),
				DisplacedPods: int32(len(loss.Displaced)),
				LostPods:      int32(len(loss.Lost)),
			}
			for _, p := range placed {
				if p.Node == "" {
					res.UnschedulablePods++
					addPod(&res, p.Pod, checksv1alpha1.PodUnschedulable, p.Reason)
				}
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
	return results, nil
}

func addPod(res *checksv1alpha1.ScenarioResult, p *corev1.Pod, issue checksv1alpha1.PodIssue, reason string) {
	if len(res.Pods) >= maxListedPods {
		res.OmittedPods++
		return
	}
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen-3] + "..."
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
	// start these w/ the manager -> first run doesn't wait, missing RBAC fails at startup
	for _, obj := range []client.Object{&corev1.Node{}, &corev1.Pod{}, &batchv1.Job{}} {
		if _, err := mgr.GetCache().GetInformer(context.Background(), obj); err != nil {
			return err
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		// skip status-only updates, else every status write kicks off a run
		For(&checksv1alpha1.ZoneCheck{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("zonecheck").
		Complete(r)
}
