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

// Package v1은 platform v1 API group의 API schema 정의 포함
// +kubebuilder:object:generate=true
// +groupName=platform.lkhun9311.github.io
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// 이 object들을 등록하는 데 쓰이는 group version,
	// applyconfiguration 생성기(예: controller-gen)가 이 이름을 사용한다.
	SchemeGroupVersion = schema.GroupVersion{Group: "platform.lkhun9311.github.io", Version: "v1"}

	// 하위 호환을 위한 SchemeGroupVersion의 별칭
	GroupVersion = SchemeGroupVersion

	// go type을 GroupVersionKind scheme에 추가하는 데 사용
	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}

	// 이 group-version의 type들을 주어진 scheme에 추가
	AddToScheme = SchemeBuilder.AddToScheme
)
