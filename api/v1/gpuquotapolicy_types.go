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

// GPUQuotaPolicy의 원하는 상태 정의
type GPUQuotaPolicySpec struct {
	// 이 정책이 적용되는 논리적 tenant (팀/조직),
	// tenant는 여러 namespace를 소유할 수 있어 targetNamespace와는 별개다.
	//
	// +required
	Tenant string `json:"tenant"`

	// quota object가 동기화되는 namespace,
	// 이 값은 불변이며 정책은 수명 동안 정확히 하나의 namespace에만 quota를 강제한다,
	// 값을 바꾸면 예전 namespace에 이미 동기화된 ResourceQuota가 고아가 된다 (reconciler는 여기 명시된 namespace만 처리한다),
	// 따라서 migration은 이 field를 바꾸지 않고 정책을 삭제한 뒤 재생성하는 방식으로 처리한다.
	//
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetNamespace is immutable"
	TargetNamespace string `json:"targetNamespace"`

	// 이 정책이 대상으로 하는 GPU class를 기록한다 (예: "l40s"),
	// 비어 있으면 전체 class를 대상으로 한다,
	// class별 quota 한정은 아직 강제되지 않으며,
	// 이 milestone에서는 class와 무관하게 단일 집계 상한(requests.nvidia.com/gpu)만 제한한다,
	// 이 field는 class별 resource key를 강제할 후속 milestone을 위해 기록해 둔다 (reconciler의 gpuClass 주석 참고).
	//
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// 대상 namespace에서 이 tenant에 적용되는 quota 상한
	//
	// +required
	Limits GPUQuotaLimits `json:"limits"`

	// gateway에서 강제하는 tenant별 서빙 요청 속도 상한 (admission Layer 4),
	// 선택값이며 rateLimit이 없는 정책은 해당 tenant에 gateway 속도 제한이 없다는 뜻이다.
	//
	// +optional
	RateLimit *GPUQuotaRateLimit `json:"rateLimit,omitempty"`
}

// 서빙 gateway가 사용하는 tenant별 token bucket 설정
type GPUQuotaRateLimit struct {
	// tenant에 허용되는 지속 요청 속도
	//
	// +kubebuilder:validation:Minimum=1
	RequestsPerMinute int32 `json:"requestsPerMinute"`

	// 지속 속도를 넘어 bucket이 순간적으로 허용하는 최대 burst
	//
	// +kubebuilder:validation:Minimum=1
	Burst int32 `json:"burst"`
}

// tenant의 quota 상한
type GPUQuotaLimits struct {
	// 허용되는 최대 GPU 수 (nvidia.com/gpu),
	// local에서는 실제 hardware가 아니라 시뮬레이션된 용량을 기준으로 한다.
	//
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`
}

// GPUQuotaPolicy의 관찰된 상태 정의
type GPUQuotaPolicyStatus struct {
	// 정책의 상위 수준 동기화 상태
	//
	// +kubebuilder:validation:Enum=Pending;Synced;Degraded
	// +optional
	Phase string `json:"phase,omitempty"`

	// controller가 마지막으로 관찰한 generation
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase가 마지막으로 바뀐 시각
	//
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// GPUQuotaPolicy resource의 현재 상태 표현,
	// 각 condition의 status는 True, False, Unknown 중 하나다.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.targetNamespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// gpuquotapolicies API의 schema
type GPUQuotaPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// 표준 object metadata
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// GPUQuotaPolicy의 원하는 상태 정의
	//
	// +required
	Spec GPUQuotaPolicySpec `json:"spec"`

	// GPUQuotaPolicy의 관찰된 상태 정의
	//
	// +optional
	Status GPUQuotaPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// GPUQuotaPolicy 목록 포함
type GPUQuotaPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []GPUQuotaPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUQuotaPolicy{}, &GPUQuotaPolicyList{})
}
