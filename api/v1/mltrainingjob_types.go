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

// MLTrainingJob의 원하는 상태 정의
type MLTrainingJobSpec struct {
	// 이 job이 admission을 거치는 Kueue LocalQueue 이름 (같은 namespace)
	//
	// +required
	Queue string `json:"queue"`

	// 학습 container image
	//
	// +required
	Image string `json:"image"`

	// container entrypoint 재정의
	//
	// +optional
	Command []string `json:"command,omitempty"`

	// 예시용 GPU class (예: "l40s"),
	// local에서는 시뮬레이션된 용량을 기준으로 동작한다 (개발 runbook 참고).
	//
	// +optional
	GPUClass string `json:"gpuClass,omitempty"`

	// pod당 GPU 수 (nvidia.com/gpu),
	// local에서는 실제 hardware가 아니라 시뮬레이션된 용량을 기준으로 한다.
	//
	// +kubebuilder:validation:Minimum=0
	// +required
	GPUCount int32 `json:"gpuCount"`

	// batch/v1 Job의 parallelism (동시 실행 pod 수)
	//
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Parallelism int32 `json:"parallelism,omitempty"`

	// batch/v1 Job의 completions (성공해야 하는 pod 수)
	//
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Completions int32 `json:"completions,omitempty"`
}

// MLTrainingJob의 관찰된 상태 정의
type MLTrainingJobStatus struct {
	// Kueue admission과 실행 수명주기 추적
	//
	// +kubebuilder:validation:Enum=Pending;Admitted;Running;Succeeded;Failed
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

	// MLTrainingJob resource의 현재 상태 표현,
	// 각 condition의 status는 True, False, Unknown 중 하나다.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Queue",type=string,JSONPath=`.spec.queue`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// mltrainingjobs API의 schema
type MLTrainingJob struct {
	metav1.TypeMeta `json:",inline"`

	// 표준 object metadata
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// MLTrainingJob의 원하는 상태 정의
	//
	// +required
	Spec MLTrainingJobSpec `json:"spec"`

	// MLTrainingJob의 관찰된 상태 정의
	//
	// +optional
	Status MLTrainingJobStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MLTrainingJob 목록 포함
type MLTrainingJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MLTrainingJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MLTrainingJob{}, &MLTrainingJobList{})
}
