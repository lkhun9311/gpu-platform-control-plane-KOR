//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/lkhun9311/gpu-mlops-platform-control-plane/test/utils"
)

var (
	// 테스트용으로 빌드하고 load할 manager image
	managerImage = "example.com/gpu-platform-control-plane:v0.0.1"
	// 이 suite가 CertManager를 설치했는지 여부 추적
	shouldCleanupCertManager = false
)

// 격리된 환경에서 솔루션을 검증하는 e2e 테스트 suite를 실행하고,
// 기본 setup은 Kind와 CertManager를 필요로 한다.
//
// kubectl kuberc(custom kubectl 설정 사용)를 활성화하려면 KUBECTL_KUBERC=true를 설정하고,
// kuberc는 기본적으로 비활성이며 이는 환경별로 일관된 테스트 동작을 보장하기 위함이고,
// CertManager 설치를 건너뛰려면 CERT_MANAGER_INSTALL_SKIP=true를 설정한다.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gpu-platform-control-plane e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	// TODO(user): e2e 테스트 vendor를 Kind에서 바꾸고 싶다면,
	// image가 빌드되어 사용 가능한지 확인한 뒤 아래 block을 제거한다.
	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	setupCertManager()
})

var _ = AfterSuite(func() {
	teardownCertManager()
})

// 테스트 격리를 위해 기본적으로 kubectl kuberc를 비활성화하고,
// local kubectl 설정이 테스트 동작에 영향을 주는 것을 방지하며,
// kuberc를 활성화하려면 KUBECTL_KUBERC=true를 설정한다.
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// webhook 테스트에 필요하면 CertManager를 설치하고,
// CERT_MANAGER_INSTALL_SKIP=true이거나 이미 설치돼 있으면 설치를 건너뛴다.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// 중단이나 부분 설치에 대비해 설치 전에 정리 대상으로 표시
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// 설치한 경우에만 CertManager를 제거하여,
// 우리가 설치한 것만 지우도록 보장한다.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
