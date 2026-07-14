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

// InferenceDeployment의 원하는 상태 정의
type InferenceDeploymentSpec struct {
	// 서빙할 model
	//
	// +required
	Model InferenceModel `json:"model"`

	// 서빙 runtime container image (예: "vllm/vllm-openai:v0.6.0")
	//
	// +required
	Image string `json:"image"`

	// 예시용 GPU class (예: "l40s"),
	// local에서는 시뮬레이션된 용량을 기준으로 동작한다 (개발 runbook 참고).
	//
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// replica당 GPU 수 (nvidia.com/gpu),
	// local에서는 실제 hardware가 아니라 시뮬레이션된 용량을 기준으로 한다.
	//
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`

	// 고정 서빙 replica 수,
	// 요청량에 따라 replica 수를 자동 조절하는 autoscaling은 이후 milestone에서 도입한다.
	//
	// +kubebuilder:validation:Minimum=0
	// +required
	Replicas int32 `json:"replicas"`

	// 서빙 container port
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`
}

// 서빙할 model 식별
type InferenceModel struct {
	// 논리적 model 이름
	//
	// +required
	Name string `json:"name"`

	// model 가중치가 저장된 위치 (예: "s3://bucket/model", "pvc://claim/path")
	//
	// +required
	StorageURI string `json:"storageUri"`
}

// InferenceDeployment의 관찰된 상태 정의
type InferenceDeploymentStatus struct {
	// 상위 수준 서빙 상태
	//
	// +kubebuilder:validation:Enum=Pending;Progressing;Ready;Degraded
	// +optional
	Phase string `json:"phase,omitempty"`

	// controller가 마지막으로 관찰한 generation
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// 준비 완료된 서빙 replica 수,
	// 소유한 Deployment의 status에서 준비된 replica 수를 그대로 반영한다.
	//
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// phase가 마지막으로 바뀐 시각
	//
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// InferenceDeployment resource의 현재 상태 표현,
	// 각 condition의 status는 True, False, Unknown 중 하나다.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// inferencedeployments API의 schema
type InferenceDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// 표준 object metadata
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// InferenceDeployment의 원하는 상태 정의
	//
	// +required
	Spec InferenceDeploymentSpec `json:"spec"`

	// InferenceDeployment의 관찰된 상태 정의
	//
	// +optional
	Status InferenceDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// InferenceDeployment 목록 포함
type InferenceDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []InferenceDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceDeployment{}, &InferenceDeploymentList{})
}
