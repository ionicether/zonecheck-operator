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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ScenarioType is the kind of failure to simulate.
// +kubebuilder:validation:Enum=Zone;BusiestNode;NodePool
type ScenarioType string

const (
	// ScenarioZone removes every node in one zone, once per zone.
	ScenarioZone ScenarioType = "Zone"
	// ScenarioBusiestNode removes the node whose movable pods would need the
	// most room elsewhere, measured as a share of the cluster's capacity.
	ScenarioBusiestNode ScenarioType = "BusiestNode"
	// ScenarioNodePool removes every node in a named node pool.
	ScenarioNodePool ScenarioType = "NodePool"
)

// ZoneCheckSpec defines which failures to simulate and how often.
type ZoneCheckSpec struct {
	// interval between simulation runs. Minimum 1m.
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m')",message="interval must be a duration of at least 1m, like 5m or 1h30m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// scenarios to simulate on every run.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +required
	Scenarios []Scenario `json:"scenarios"`
}

// Scenario describes one kind of failure.
// +kubebuilder:validation:XValidation:rule="self.type == 'NodePool' ? has(self.nodePool) : !has(self.nodePool)",message="nodePool is required for type NodePool and not allowed otherwise"
// +kubebuilder:validation:XValidation:rule="self.type == 'Zone' || !has(self.zoneLabel)",message="zoneLabel is only allowed for type Zone"
type Scenario struct {
	// type of failure to simulate.
	// +required
	Type ScenarioType `json:"type"`

	// zoneLabel is the node label that holds the zone. Each distinct value
	// is simulated as its own failure. Defaults to topology.kubernetes.io/zone.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:XValidation:rule="!format.qualifiedName().validate(self).hasValue()",message="zoneLabel must be a valid label key"
	// +optional
	ZoneLabel string `json:"zoneLabel,omitempty"`

	// nodePool selects the pool to remove for type NodePool.
	// +optional
	NodePool *NodePoolRef `json:"nodePool,omitempty"`
}

// ZoneLabelOrDefault returns the label to group nodes into zones by.
// Not a CRD default, since that would set it on every scenario type.
func (s Scenario) ZoneLabelOrDefault() string {
	if s.ZoneLabel == "" {
		return corev1.LabelTopologyZone
	}
	return s.ZoneLabel
}

// NodePoolRef identifies a node pool by a label key and value, since every
// provider labels pools differently.
type NodePoolRef struct {
	// labelKey is the node label that names the pool.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:XValidation:rule="!format.qualifiedName().validate(self).hasValue()",message="labelKey must be a valid label key"
	// +required
	LabelKey string `json:"labelKey"`

	// name is the label value for the pool.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.labelValue().validate(self).hasValue()",message="name must be a valid label value"
	// +required
	Name string `json:"name"`
}

// ZoneCheckStatus holds the results of the most recent run.
type ZoneCheckStatus struct {
	// observedGeneration is the spec generation the last run used.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastRunTime is when the last simulation finished.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// results has one entry per simulated failure. A Zone scenario in a
	// cluster with three zones produces three entries.
	// +listType=atomic
	// +optional
	Results []ScenarioResult `json:"results,omitempty"`

	// conditions represent the current state of the ZoneCheck.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ScenarioResult is the outcome of simulating one failure.
type ScenarioResult struct {
	// type of failure that was simulated.
	// +required
	Type ScenarioType `json:"type"`

	// target is what was removed: the zone, node, or pool name.
	// +required
	Target string `json:"target"`

	// lostNodes is how many nodes the failure removed.
	// +required
	LostNodes int32 `json:"lostNodes"`

	// displacedPods is how many pods were running on the lost nodes.
	// +required
	DisplacedPods int32 `json:"displacedPods"`

	// unschedulablePods is how many displaced pods found no new home.
	// +required
	UnschedulablePods int32 `json:"unschedulablePods"`

	// unschedulable lists the pods that found no new home.
	// +listType=atomic
	// +optional
	Unschedulable []UnschedulablePod `json:"unschedulable,omitempty"`
}

// UnschedulablePod is a pod the simulation could not place.
type UnschedulablePod struct {
	// +required
	Namespace string `json:"namespace"`

	// +required
	Name string `json:"name"`

	// owner is the controlling workload, like "Deployment/web".
	// +optional
	Owner string `json:"owner,omitempty"`

	// reason is the scheduler's explanation.
	// +required
	Reason string `json:"reason"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Last Run",type=date,JSONPath=`.status.lastRunTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZoneCheck continuously simulates node, zone, or pool failures and
// reports which workloads couldn't be rescheduled.
type ZoneCheck struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ZoneCheckSpec `json:"spec"`

	// +optional
	Status ZoneCheckStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ZoneCheckList contains a list of ZoneCheck.
type ZoneCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ZoneCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ZoneCheck{}, &ZoneCheckList{})
		return nil
	})
}
