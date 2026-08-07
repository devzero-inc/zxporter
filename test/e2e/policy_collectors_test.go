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
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/devzero-inc/zxporter/internal/collector"
)

// The policy-collectors suite drives the real Kyverno, Gatekeeper, and
// mig-parted collectors in-process against a kind cluster with real Kyverno
// and Gatekeeper installations (provisioned by
// .github/actions/policy-kind/action.yml), mirroring oom-flow-kind's and
// gpu-mig-kind's split between infra setup (shell) and test logic (Go).
// The collectors under test are exactly what the operator wires up in
// registerResourceCollectors; the assertions read their public
// GetResourceChannel output — real informers, real CRDs, real policy
// engine behavior (kyverno report generation, gatekeeper audit).

const (
	// policyE2ENamespace hosts the violating workloads the policy engines
	// should flag.
	policyE2ENamespace = "policy-collectors-e2e"

	// policyBatchTime keeps collector batches flushing fast so Eventually
	// blocks don't wait on the default 5s batcher.
	policyBatchTime = 500 * time.Millisecond

	policyBatchSize = 50
)

// collectedSink drains a collector's resource channel into a guarded slice
// so specs can assert on everything emitted so far.
type collectedSink struct {
	mu        sync.Mutex
	collected []collector.CollectedResource
}

func newCollectedSink(ch <-chan []collector.CollectedResource) *collectedSink {
	s := &collectedSink{}
	go func() {
		for batch := range ch {
			s.mu.Lock()
			s.collected = append(s.collected, batch...)
			s.mu.Unlock()
		}
	}()
	return s
}

// find returns the most recent emission matching the predicate.
func (s *collectedSink) find(pred func(collector.CollectedResource) bool) (collector.CollectedResource, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.collected) - 1; i >= 0; i-- {
		if pred(s.collected[i]) {
			return s.collected[i], true
		}
	}
	return collector.CollectedResource{}, false
}

func payloadOf(res collector.CollectedResource) map[string]interface{} {
	payload, ok := res.Object.(map[string]interface{})
	Expect(ok).To(BeTrue(), "collected object should be a map payload, got %T", res.Object)
	return payload
}

var _ = Describe("Policy collectors (Kyverno, Gatekeeper, mig-parted)", Ordered, Label("policy"), func() {
	var (
		clientset     kubernetes.Interface
		dynamicClient dynamic.Interface
		discoClient   discovery.DiscoveryInterface
	)

	BeforeAll(func() {
		cfg := ctrl.GetConfigOrDie()
		var err error
		clientset, err = kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		dynamicClient, err = dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		discoClient, err = discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: policyE2ENamespace},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterAll(func() {
		_ = clientset.CoreV1().Namespaces().Delete(context.Background(), policyE2ENamespace, metav1.DeleteOptions{})
	})

	Describe("Kyverno policy and policy report collectors", func() {
		var (
			policyCollector *collector.KyvernoPolicyCollector
			reportCollector *collector.KyvernoPolicyReportCollector
			policySink      *collectedSink
			reportSink      *collectedSink
		)

		clusterPolicyGVR := schema.GroupVersionResource{
			Group: "kyverno.io", Version: "v1", Resource: "clusterpolicies",
		}
		const policyName = "e2e-require-team-label"

		BeforeAll(func() {
			ctx := context.Background()

			By("starting the Kyverno policy and policy report collectors")
			policyCollector = collector.NewKyvernoPolicyCollector(
				dynamicClient, nil, policyBatchSize, policyBatchTime, logr.Discard(), nil)
			Expect(policyCollector.IsAvailable(ctx)).To(BeTrue(),
				"kyverno CRDs must be installed by the CI action before this suite runs")
			Expect(policyCollector.Start(ctx)).To(Succeed())
			policySink = newCollectedSink(policyCollector.GetResourceChannel())

			reportCollector = collector.NewKyvernoPolicyReportCollector(
				dynamicClient, nil, policyBatchSize, policyBatchTime, logr.Discard(), nil)
			Expect(reportCollector.IsAvailable(ctx)).To(BeTrue())
			Expect(reportCollector.Start(ctx)).To(Succeed())
			reportSink = newCollectedSink(reportCollector.GetResourceChannel())

			By("creating an Audit ClusterPolicy requiring a team label")
			clusterPolicy := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "kyverno.io/v1",
				"kind":       "ClusterPolicy",
				"metadata":   map[string]interface{}{"name": policyName},
				"spec": map[string]interface{}{
					"validationFailureAction": "Audit",
					"background":              true,
					"rules": []interface{}{
						map[string]interface{}{
							"name": "check-team-label",
							"match": map[string]interface{}{
								"any": []interface{}{
									map[string]interface{}{
										"resources": map[string]interface{}{
											"kinds":      []interface{}{"Pod"},
											"namespaces": []interface{}{policyE2ENamespace},
										},
									},
								},
							},
							"validate": map[string]interface{}{
								"message": "the team label is required",
								"pattern": map[string]interface{}{
									"metadata": map[string]interface{}{
										"labels": map[string]interface{}{
											"team": "?*",
										},
									},
								},
							},
						},
					},
				},
			}}
			_, err := dynamicClient.Resource(clusterPolicyGVR).Create(ctx, clusterPolicy, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("creating a pod that violates the policy (no team label)")
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "e2e-unlabeled-pod", Namespace: policyE2ENamespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "sleeper",
						Image:   "busybox:1.36",
						Command: []string{"sleep", "600"},
					}},
				},
			}
			_, err = clientset.CoreV1().Pods(policyE2ENamespace).Create(ctx, pod, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterAll(func() {
			_ = dynamicClient.Resource(clusterPolicyGVR).Delete(context.Background(), policyName, metav1.DeleteOptions{})
			_ = policyCollector.Stop()
			_ = reportCollector.Stop()
		})

		It("collects the ClusterPolicy with its parsed spec fields", func() {
			Eventually(func() bool {
				res, found := policySink.find(func(r collector.CollectedResource) bool {
					return r.ResourceType == collector.KyvernoPolicy && r.Key == policyName
				})
				if !found {
					return false
				}
				payload := payloadOf(res)
				return payload["kind"] == "ClusterPolicy" &&
					payload["validationFailureAction"] == "Audit" &&
					payload["ruleCount"] == 1
			}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
				"the ClusterPolicy should be collected with kind, failure action, and rule count")
		})

		It("collects a PolicyReport recording the violation", func() {
			// Kyverno's report controllers aggregate admission/background scan
			// results into per-resource PolicyReports asynchronously.
			Eventually(func() bool {
				res, found := reportSink.find(func(r collector.CollectedResource) bool {
					if r.ResourceType != collector.KyvernoPolicyReport {
						return false
					}
					payload := payloadOf(r)
					if payload["namespace"] != policyE2ENamespace {
						return false
					}
					summary, _ := payload["summary"].(map[string]interface{})
					fail, _ := summary["fail"].(int64)
					return fail > 0
				})
				if !found {
					return false
				}
				payload := payloadOf(res)
				results, _ := payload["results"].([]interface{})
				for _, entry := range results {
					result, _ := entry.(map[string]interface{})
					if result["policy"] == policyName && result["result"] == "fail" {
						return true
					}
				}
				return false
			}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
				"a PolicyReport with a fail result for the unlabeled pod should be collected")
		})
	})

	Describe("Gatekeeper constraint template and constraint collectors", func() {
		var (
			templateCollector   *collector.GatekeeperConstraintTemplateCollector
			constraintCollector *collector.GatekeeperConstraintCollector
			templateSink        *collectedSink
			constraintSink      *collectedSink
		)

		templateGVR := schema.GroupVersionResource{
			Group: "templates.gatekeeper.sh", Version: "v1", Resource: "constrainttemplates",
		}
		constraintGVR := schema.GroupVersionResource{
			Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: "e2erequiredlabels",
		}
		const templateName = "e2erequiredlabels"
		const constraintName = "e2e-ns-must-have-team"

		BeforeAll(func() {
			ctx := context.Background()

			By("starting the constraint template collector")
			templateCollector = collector.NewGatekeeperConstraintTemplateCollector(
				dynamicClient, policyBatchSize, policyBatchTime, logr.Discard(), nil)
			Expect(templateCollector.IsAvailable(ctx)).To(BeTrue(),
				"gatekeeper must be installed by the CI action before this suite runs")
			Expect(templateCollector.Start(ctx)).To(Succeed())
			templateSink = newCollectedSink(templateCollector.GetResourceChannel())

			By("creating a ConstraintTemplate")
			template := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "templates.gatekeeper.sh/v1",
				"kind":       "ConstraintTemplate",
				"metadata":   map[string]interface{}{"name": templateName},
				"spec": map[string]interface{}{
					"crd": map[string]interface{}{
						"spec": map[string]interface{}{
							"names": map[string]interface{}{
								"kind": "E2ERequiredLabels",
							},
							"validation": map[string]interface{}{
								"openAPIV3Schema": map[string]interface{}{
									"type": "object",
									"properties": map[string]interface{}{
										"labels": map[string]interface{}{
											"type":  "array",
											"items": map[string]interface{}{"type": "string"},
										},
									},
								},
							},
						},
					},
					"targets": []interface{}{
						map[string]interface{}{
							"target": "admission.k8s.gatekeeper.sh",
							"rego": `package e2erequiredlabels

violation[{"msg": msg}] {
  required := input.parameters.labels[_]
  not input.review.object.metadata.labels[required]
  msg := sprintf("missing required label: %v", [required])
}`,
						},
					},
				},
			}}
			_, err := dynamicClient.Resource(templateGVR).Create(ctx, template, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("waiting for gatekeeper to create the constraint CRD")
			Eventually(func() bool {
				_, listErr := dynamicClient.Resource(constraintGVR).List(ctx, metav1.ListOptions{Limit: 1})
				return listErr == nil
			}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
				"the E2ERequiredLabels constraint CRD should become servable")

			// The constraint collector starts after the template's CRD exists
			// so initial discovery finds it; late-arriving kinds are covered
			// by the collector's periodic re-discovery (unit-tested in
			// gatekeeper_constraint_collector_test.go — too slow for e2e).
			By("starting the constraint collector")
			constraintCollector = collector.NewGatekeeperConstraintCollector(
				dynamicClient, discoClient, policyBatchSize, policyBatchTime, logr.Discard(), nil)
			Expect(constraintCollector.Start(ctx)).To(Succeed())
			constraintSink = newCollectedSink(constraintCollector.GetResourceChannel())

			By("creating a warn-mode constraint requiring a team label on namespaces")
			constraint := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "constraints.gatekeeper.sh/v1beta1",
				"kind":       "E2ERequiredLabels",
				"metadata":   map[string]interface{}{"name": constraintName},
				"spec": map[string]interface{}{
					"enforcementAction": "warn",
					"match": map[string]interface{}{
						"kinds": []interface{}{
							map[string]interface{}{
								"apiGroups": []interface{}{""},
								"kinds":     []interface{}{"Namespace"},
							},
						},
					},
					"parameters": map[string]interface{}{
						"labels": []interface{}{"e2e-team"},
					},
				},
			}}
			_, err = dynamicClient.Resource(constraintGVR).Create(ctx, constraint, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterAll(func() {
			_ = dynamicClient.Resource(constraintGVR).Delete(context.Background(), constraintName, metav1.DeleteOptions{})
			_ = dynamicClient.Resource(templateGVR).Delete(context.Background(), templateName, metav1.DeleteOptions{})
			if templateCollector != nil {
				_ = templateCollector.Stop()
			}
			if constraintCollector != nil {
				_ = constraintCollector.Stop()
			}
		})

		It("collects the ConstraintTemplate with its generated CRD kind", func() {
			Eventually(func() bool {
				res, found := templateSink.find(func(r collector.CollectedResource) bool {
					return r.ResourceType == collector.GatekeeperConstraintTemplate &&
						r.Key == fmt.Sprintf("constrainttemplates/%s", templateName)
				})
				if !found {
					return false
				}
				payload := payloadOf(res)
				return payload["crdKind"] == "E2ERequiredLabels"
			}, 2*time.Minute, 2*time.Second).Should(BeTrue())
		})

		It("collects the constraint and its audit violations", func() {
			// The policy-collectors-e2e namespace itself has no e2e-team
			// label, so gatekeeper's audit (auditInterval=10 set by the CI
			// action) reports it as a violation on the constraint status.
			Eventually(func() bool {
				res, found := constraintSink.find(func(r collector.CollectedResource) bool {
					if r.ResourceType != collector.GatekeeperConstraint {
						return false
					}
					payload := payloadOf(r)
					return payload["kind"] == "E2ERequiredLabels" && payload["name"] == constraintName
				})
				if !found {
					return false
				}
				payload := payloadOf(res)
				total, _ := payload["totalViolations"].(int64)
				return payload["enforcementAction"] == "warn" && total > 0
			}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
				"the constraint should be collected with audit violations recorded")
		})
	})

	Describe("mig-parted config collector", func() {
		var (
			migCollector *collector.MigPartedConfigCollector
			migSink      *collectedSink
		)

		const migConfigYAML = `version: v1
mig-configs:
  all-disabled:
    - devices: all
      mig-enabled: false
  all-1g.5gb:
    - devices: all
      mig-enabled: true
      mig-devices:
        1g.5gb: 7
`

		BeforeAll(func() {
			ctx := context.Background()

			By("creating the gpu-operator namespace and mig-parted ConfigMap")
			_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: collector.DefaultMigPartedConfigMapNamespace},
			}, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      collector.DefaultMigPartedConfigMapName,
					Namespace: collector.DefaultMigPartedConfigMapNamespace,
				},
				Data: map[string]string{"config.yaml": migConfigYAML},
			}
			_, err = clientset.CoreV1().ConfigMaps(collector.DefaultMigPartedConfigMapNamespace).
				Create(ctx, cm, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			By("starting the mig-parted config collector")
			migCollector = collector.NewMigPartedConfigCollector(
				clientset, "", "", policyBatchSize, policyBatchTime, logr.Discard(), nil)
			Expect(migCollector.Start(ctx)).To(Succeed())
			migSink = newCollectedSink(migCollector.GetResourceChannel())
		})

		AfterAll(func() {
			_ = clientset.CoreV1().ConfigMaps(collector.DefaultMigPartedConfigMapNamespace).Delete(
				context.Background(), collector.DefaultMigPartedConfigMapName, metav1.DeleteOptions{})
			_ = clientset.CoreV1().Namespaces().Delete(
				context.Background(), collector.DefaultMigPartedConfigMapNamespace, metav1.DeleteOptions{})
			if migCollector != nil {
				_ = migCollector.Stop()
			}
		})

		It("collects the mig-parted ConfigMap with parsed MIG profiles", func() {
			Eventually(func() bool {
				res, found := migSink.find(func(r collector.CollectedResource) bool {
					return r.ResourceType == collector.MigPartedConfig
				})
				if !found {
					return false
				}
				payload := payloadOf(res)
				migConfigs, _ := payload["migConfigs"].(map[string]interface{})
				return payload["version"] == "v1" && len(migConfigs) == 2
			}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
				"the mig-parted config should be collected with both parsed profiles")
		})

		It("re-collects the ConfigMap when its profiles change", func() {
			ctx := context.Background()

			By("adding a profile to the ConfigMap")
			cm, err := clientset.CoreV1().ConfigMaps(collector.DefaultMigPartedConfigMapNamespace).Get(
				ctx, collector.DefaultMigPartedConfigMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			cm.Data["config.yaml"] = migConfigYAML + `  all-3g.20gb:
    - devices: all
      mig-enabled: true
      mig-devices:
        3g.20gb: 2
`
			_, err = clientset.CoreV1().ConfigMaps(collector.DefaultMigPartedConfigMapNamespace).Update(
				ctx, cm, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				res, found := migSink.find(func(r collector.CollectedResource) bool {
					if r.ResourceType != collector.MigPartedConfig || r.EventType != collector.EventTypeUpdate {
						return false
					}
					payload := payloadOf(r)
					migConfigs, _ := payload["migConfigs"].(map[string]interface{})
					return len(migConfigs) == 3
				})
				return found && res.EventType == collector.EventTypeUpdate
			}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
				"the updated config with three profiles should be re-collected")
		})
	})
})
