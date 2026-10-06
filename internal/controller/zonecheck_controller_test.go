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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	checksv1alpha1 "github.com/ionicether/zonecheck-operator/api/v1alpha1"
)

var _ = Describe("ZoneCheck Controller", func() {
	ctx := context.Background()

	Context("When reconciling a resource", func() {
		const name = "test-resource"
		key := types.NamespacedName{Name: name}
		check := func() *checksv1alpha1.ZoneCheck {
			return &checksv1alpha1.ZoneCheck{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: checksv1alpha1.ZoneCheckSpec{
					Scenarios: []checksv1alpha1.Scenario{{Type: checksv1alpha1.ScenarioZone}},
				},
			}
		}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, check())).To(Succeed())
		})

		AfterEach(func() {
			Expect(k8sClient.Delete(ctx, check())).To(Succeed())
		})

		It("applies defaults", func() {
			zc := &checksv1alpha1.ZoneCheck{}
			Expect(k8sClient.Get(ctx, key, zc)).To(Succeed())
			Expect(zc.Spec.Interval.Duration).To(Equal(5 * time.Minute))
			Expect(zc.Spec.Scenarios[0].ZoneLabelOrDefault()).To(Equal(corev1.LabelTopologyZone))
		})

		It("reconciles without error", func() {
			r := &ZoneCheckReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Validation", func() {
		createRaw := func(spec map[string]any) error {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "checks.zonecheck.dev/v1alpha1",
				"kind":       "ZoneCheck",
				"metadata":   map[string]any{"name": "validation"},
				"spec":       spec,
			}}
			err := k8sClient.Create(ctx, u)
			if err == nil {
				Expect(k8sClient.Delete(ctx, u)).To(Succeed())
			}
			return err
		}
		zoneOnly := []any{map[string]any{"type": "Zone"}}

		It("requires scenarios", func() {
			err := createRaw(map[string]any{"interval": "5m"})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(err).To(MatchError(ContainSubstring("spec.scenarios")))
			Expect(createRaw(map[string]any{"scenarios": []any{}})).To(MatchError(ContainSubstring("spec.scenarios")))
		})

		DescribeTable("checks the interval",
			func(interval string, ok bool) {
				err := createRaw(map[string]any{"interval": interval, "scenarios": zoneOnly})
				if !ok {
					Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
					Expect(err).To(MatchError(ContainSubstring("interval")))
					return
				}
				Expect(err).NotTo(HaveOccurred())
				// Whatever the API server accepts, the Go client has to parse.
				_, err = time.ParseDuration(interval)
				Expect(err).NotTo(HaveOccurred())
			},
			Entry("1m is the minimum", "1m", true),
			Entry("compound", "1h30m", true),
			Entry("fractional", "1.5h", true),
			Entry("seconds", "90s", true),
			Entry("zero", "0s", false),
			Entry("negative", "-5m", false),
			Entry("too short", "30s", false),
			Entry("microseconds", "1µs", false),
			Entry("not a duration", "banana", false),
			Entry("days aren't a Go duration", "1d", false),
			Entry("units are case sensitive", "5M", false),
		)

		DescribeTable("checks scenarios",
			func(scenario map[string]any, wantErr string) {
				err := createRaw(map[string]any{"scenarios": []any{scenario}})
				if wantErr == "" {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(MatchError(ContainSubstring(wantErr)))
				}
			},
			Entry("valid pool", map[string]any{"type": "NodePool",
				"nodePool": map[string]any{"labelKey": "example.com/pool", "name": "gpu"}}, ""),
			Entry("pool missing", map[string]any{"type": "NodePool"}, "nodePool is required"),
			Entry("pool on another type", map[string]any{"type": "BusiestNode",
				"nodePool": map[string]any{"labelKey": "pool", "name": "a"}}, "nodePool is required"),
			Entry("zoneLabel on another type", map[string]any{"type": "BusiestNode", "zoneLabel": "zone"},
				"zoneLabel is only allowed"),
			Entry("bad zone label", map[string]any{"type": "Zone", "zoneLabel": "bad key!"}, "valid label key"),
			Entry("bad pool key", map[string]any{"type": "NodePool",
				"nodePool": map[string]any{"labelKey": "a/b/c", "name": "gpu"}}, "valid label key"),
			Entry("bad pool name", map[string]any{"type": "NodePool",
				"nodePool": map[string]any{"labelKey": "pool", "name": "has space"}}, "valid label value"),
		)

	})
})
