/*
Copyright 2026.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 이 파일은 직접 고쳐 가며 채워 넣는 기본 골격이다
// 참고: json tag는 필수, 새 field를 추가할 때 직렬화되려면 반드시 json tag를 붙일 것

// NodeHealth의 원하는 상태 정의
type NodeHealthSpec struct {
	// 이 resource가 추적하는 대상 Node object의 이름,
	// 이 값은 불변이며 하나의 NodeHealth는 수명 동안 정확히 한 node의 unhealthy taint만 관리한다,
	// 정리 logic은 항상 여기 명시된 node만 대상으로 하므로,
	// 값을 바꾸면 이전 node에 이미 붙은 taint가 고아가 된다.
	//
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeName is immutable"
	NodeName string `json:"nodeName"`

	// node의 GPU class 예시값 (예: "l40s"),
	// local에서는 시뮬레이션된 용량으로 대체한다 (개발 runbook 참고).
	//
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`
}

// NodeHealth의 관찰된 상태 정의
type NodeHealthStatus struct {
	// node의 상위 수준 건강 상태,
	// M3에서는 Pending, Ready, Quarantine만 내보낸다,
	// Intake와 Degraded는 node 반입과 성능 저하 수명주기 단계용으로 예약되어 있으며 아직 내보내지 않는다 (docs/03 참고).
	//
	// +kubebuilder:validation:Enum=Pending;Intake;Ready;Degraded;Quarantine
	// +optional
	Phase string `json:"phase,omitempty"`

	// controller가 마지막으로 관찰한 generation
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// 현재 결함 신호의 출처를 기록한다,
	// local에서는 실제 hardware 신호가 아니라 시뮬레이션된 신호이므로 그대로 정직하게 남겨 둔다.
	//
	// +optional
	FaultSignal *FaultSignal `json:"faultSignal,omitempty"`

	// phase가 마지막으로 바뀐 시각
	//
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// NodeHealth resource의 현재 상태 표현,
	// 각 condition의 status는 True, False, Unknown 중 하나다.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// node 결함 신호가 어디서 발생했는지 기록
type FaultSignal struct {
	// 신호의 출처 (예: "simulated")
	//
	// +optional
	Source string `json:"source,omitempty"`

	// 선택적 결함 code
	//
	// +optional
	Code string `json:"code,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// nodehealths API의 schema
type NodeHealth struct {
	metav1.TypeMeta `json:",inline"`

	// 표준 object metadata
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// NodeHealth의 원하는 상태 정의
	//
	// +required
	Spec NodeHealthSpec `json:"spec"`

	// NodeHealth의 관찰된 상태 정의
	//
	// +optional
	Status NodeHealthStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NodeHealth 목록 포함
type NodeHealthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NodeHealth `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeHealth{}, &NodeHealthList{})
}
