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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	checksv1alpha1 "github.com/ionicether/zonecheck-operator/api/v1alpha1"
)

// ZoneCheckReconciler runs the simulations for each ZoneCheck.
type ZoneCheckReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=checks.zonecheck.dev,resources=zonechecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checks.zonecheck.dev,resources=zonechecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checks.zonecheck.dev,resources=zonechecks/finalizers,verbs=update

func (r *ZoneCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO: snapshot the cluster and simulate each scenario

	return ctrl.Result{}, nil
}

func (r *ZoneCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checksv1alpha1.ZoneCheck{}).
		Named("zonecheck").
		Complete(r)
}
