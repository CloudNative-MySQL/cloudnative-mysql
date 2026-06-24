//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec exercises the MySQL major-version-upgrade admission guard
// (ClusterSpecValidator). It does not roll a real cross-version upgrade — the e2e
// matrix pins a single server version per run, so an end-to-end 8.0 -> 8.4 roll
// needs a multi-series image matrix and is tracked separately. Here we prove the
// validating webhook rejects unsupported transitions at apply time.

// upgradeCatalogName is the ImageCatalog these specs resolve series against.
const upgradeCatalogName = "upgrade-images"

func upgradeCatalogManifest(name, ns string) string {
	// The images do not need to be pullable: the webhook validates the spec
	// (series transitions), not image contents, and these specs are deleted
	// without waiting for readiness.
	return fmt.Sprintf(`apiVersion: mysql.cnmsql.co/v1alpha1
kind: ImageCatalog
metadata:
  name: %s
  namespace: %s
spec:
  images:
    - series: "8.0"
      image: %s
    - series: "8.4"
      image: %s
    - series: "9.0"
      image: %s
`, name, ns, instanceImage, instanceImage, instanceImage)
}

func catalogClusterManifest(name, ns, series string) string {
	return fmt.Sprintf(`apiVersion: mysql.cnmsql.co/v1alpha1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  instances: 1
  imageCatalogRef:
    apiGroup: mysql.cnmsql.co
    kind: ImageCatalog
    name: %s
    series: "%s"
  storage:
    size: 1Gi
  mysql:
%s
%s
  bootstrap:
    initdb:
      database: app
      owner: app
`, name, ns, upgradeCatalogName, series, e2eMySQLParameters, e2eInstanceResources)
}

// expectApplyRejected applies a manifest expecting the admission webhook to deny
// it, and asserts the denial message contains want.
func expectApplyRejected(name, manifest, want string) {
	path := writeManifest(name, manifest)
	out, err := kubectl("apply", "-f", path)
	Expect(err).To(HaveOccurred(), "expected the apply to be rejected by the webhook")
	Expect(strings.ToLower(out)).To(ContainSubstring(strings.ToLower(want)),
		"rejection message should explain why")
}

var _ = Describe("MySQL major-version upgrade admission", Ordered, func() {
	const cluster = "upgrade-guard"

	BeforeAll(func() {
		applyManifest(upgradeCatalogName, upgradeCatalogManifest(upgradeCatalogName, testNamespace))
	})

	AfterAll(func() {
		deleteManifest(upgradeCatalogName, upgradeCatalogManifest(upgradeCatalogName, testNamespace))
	})

	It("rejects a cluster that sets both imageName and imageCatalogRef on create", func() {
		manifest := strings.Replace(
			catalogClusterManifest("upgrade-both", testNamespace, "8.0"),
			"  storage:",
			"  imageName: "+instanceImage+"\n  storage:", 1)
		expectApplyRejected("upgrade-both", manifest, "mutually exclusive")
	})

	It("rejects a skipped series and allows a single hop", func() {
		By("creating a cluster pinned to series 8.0")
		applyManifest(cluster, catalogClusterManifest(cluster, testNamespace, "8.0"))
		DeferCleanup(func() { deleteCluster(cluster) })

		By("rejecting a skip straight to 9.0")
		expectApplyRejected(cluster, catalogClusterManifest(cluster, testNamespace, "9.0"), "8.4")

		By("allowing the adjacent hop to 8.4")
		applyManifest(cluster, catalogClusterManifest(cluster, testNamespace, "8.4"))

		By("rejecting a downgrade back to 8.0")
		expectApplyRejected(cluster, catalogClusterManifest(cluster, testNamespace, "8.0"), "downgrade")
	})
})
