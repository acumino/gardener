// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodelocaldns_test

import (
	"context"
	"strconv"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	. "github.com/gardener/gardener/pkg/operation/botanist/component/nodelocaldns"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/retry"
	retryfake "github.com/gardener/gardener/pkg/utils/retry/fake"
	"github.com/gardener/gardener/pkg/utils/test"
	. "github.com/gardener/gardener/pkg/utils/test/matchers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("NodeLocalDNS", func() {
	var (
		ctx = context.TODO()

		managedResourceName = "shoot-core-nodelocaldns"
		namespace           = "some-namespace"
		image               = "some-image:some-tag"
		nodeLocal           = common.NodeLocalIPVSAddress
		prometheusPort      = 9253
		prometheusScrape    = true

		c         client.Client
		values    Values
		component component.DeployWaiter

		managedResource       *resourcesv1alpha1.ManagedResource
		managedResourceSecret *corev1.Secret

		configMapHash string
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(kubernetes.SeedScheme).Build()
		values = Values{
			Image: image,
		}

		component = New(c, namespace, values)

		managedResource = &resourcesv1alpha1.ManagedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name:      managedResourceName,
				Namespace: namespace,
			},
		}
		managedResourceSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "managedresource-" + managedResource.Name,
				Namespace: namespace,
			},
		}

	})

	Describe("#Deploy", func() {
		var (
			serviceAccountYAML = `apiVersion: v1
kind: ServiceAccount
metadata:
  creationTimestamp: null
  name: node-local-dns
  namespace: kube-system
`
			podSecurityPolicyYAML = `apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  creationTimestamp: null
  labels:
    app: node-local-dns
  name: gardener.kube-system.node-local-dns
spec:
  allowedHostPaths:
  - pathPrefix: /run/xtables.lock
  fsGroup:
    rule: RunAsAny
  hostNetwork: true
  hostPorts:
  - max: 53
    min: 53
  - max: 9253
    min: 9253
  privileged: true
  runAsUser:
    rule: RunAsAny
  seLinux:
    rule: RunAsAny
  supplementalGroups:
    rule: RunAsAny
  volumes:
  - secret
  - hostPath
  - configMap
`
			clusterRoleYAML = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  creationTimestamp: null
  labels:
    app: node-local-dns
  name: gardener.cloud:psp:kube-system:node-local-dns
rules:
- apiGroups:
  - policy
  - extensions
  resourceNames:
  - gardener.kube-system.node-local-dns
  resources:
  - podsecuritypolicies
  verbs:
  - use
`
			roleBindingYAML = `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  annotations:
    resources.gardener.cloud/delete-on-invalid-update: "true"
  creationTimestamp: null
  labels:
    app: node-local-dns
  name: gardener.cloud:psp:node-local-dns
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: gardener.cloud:psp:kube-system:node-local-dns
subjects:
- kind: ServiceAccount
  name: node-local-dns
  namespace: kube-system
`
			configMapYAML = func() string { //todo cluster.local and space between the bind

				out := `apiVersion: v1
data:
  Corefile: |
    cluster.local:53 {
      errors
      cache {
              success 9984 30
              denial 9984 5
      }
      reload
      loop
      bind ` + nodeLocal + values.DNSServer + `
      forward . ` + values.ClusterDNS + ` {
              ` + forceTcpToClusterDNS(values) + `
      }
      prometheus :` + strconv.Itoa(prometheusPort) + `
      health ` + nodeLocal + `:8080
      }
    in-addr.arpa:53 {
      errors
      cache 30
      reload
      loop
      bind ` + nodeLocal + values.DNSServer + `
      forward . ` + values.ClusterDNS + ` {
              ` + forceTcpToClusterDNS(values) + `
      }
      prometheus :` + strconv.Itoa(prometheusPort) + `
      }
    .ip6.arpa:53 {
      errors
      cache 30
      reload
      loop
      bind ` + nodeLocal + values.DNSServer + `
      forward . ` + values.ClusterDNS + ` {
              ` + forceTcpToClusterDNS(values) + `
      }
      prometheus :` + strconv.Itoa(prometheusPort) + `
      }
    .53 {
      errors
      cache 30
      reload
      loop
      bind ` + nodeLocal + values.DNSServer + `
      forward . __PILLAR__UPSTREAM__SERVERS__ {
              ` + forceTcpToUpstreamDNS(values) + `
      }
      prometheus :` + strconv.Itoa(prometheusPort) + `
      }
    }
immutable: true
kind: ConfigMap
metadata:
  creationTimestamp: null
  labels:
    resources.gardener.cloud/garbage-collectable-reference: "true"
  name: node-local-dns-` + configMapHash + `
  namespace: kube-system
`

				return out

			}
			serviceYAML = `apiVersion: v1
kind: Service
metadata:
  creationTimestamp: null
  labels:
    k8s-app: kube-dns-upstream
    kubernetes.io/cluster-service: "true"
    kubernetes.io/name: NodeLocalDNS
  name: kube-dns-upstream
  namespace: kube-system
spec:
  ports:
  - name: dns
    port: 53
    protocol: UDP
    targetPort: 8053
  - name: dns-tcp
    port: 53
    protocol: TCP
    targetPort: 8053
  selector:
    k8s-app: kube-dns
status:
  loadBalancer: {}
`
			daemonsetYAMLFor = func() string {
				out := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  annotations:
    reference.resources.gardener.cloud/configmap-` + utils.ComputeSHA256Hex([]byte(`kube-dns`))[:8] + `: kube-dns
    reference.resources.gardener.cloud/configmap-` + utils.ComputeSHA256Hex([]byte(`node-local-dns-` + configMapHash))[:8] + `: node-local-dns-` + configMapHash + `
  creationTimestamp: null
  labels:
    gardener.cloud/role: system-component
    k8s-app: node-local-dns
    origin: gardener
  name: node-local-dns
  namespace: kube-system
spec:
  selector:
    matchLabels:
      k8s-app: node-local-dns
  template:
    metadata:
      annotations:
        prometheus.io/port: "` + strconv.Itoa(prometheusPort) + `"
        prometheus.io/scrape: "` + strconv.FormatBool(prometheusScrape) + `"
        reference.resources.gardener.cloud/configmap-` + utils.ComputeSHA256Hex([]byte(`kube-dns`))[:8] + `: kube-dns
        reference.resources.gardener.cloud/configmap-` + utils.ComputeSHA256Hex([]byte(`node-local-dns-` + configMapHash))[:8] + `: node-local-dns-` + configMapHash + `
      creationTimestamp: null
      labels:
        k8s-app: node-local-dns
        networking.gardener.cloud/to-dns: allowed
    spec:
      containers:
      - args:
        - -localip
`
				out += `        - ` + containerArg(values)
				out += `
        - -conf
        - '"/etc/Corefile"'
        - -upstreamsvc
        - kube-dns-updtream
        image: ` + values.Image + `
        livenessProbe:
          httpGet:
            host: ` + nodeLocal + `
            path: /health
            port: 8080
          initialDelaySeconds: 60
          timeoutSeconds: 5
        name: node-cache
        ports:
        - containerPort: 53
          name: dns
          protocol: UDP
        - containerPort: 53
          name: dns-tcp
          protocol: TCP
        - containerPort: 9253
          name: metrics
          protocol: TCP
        resources:
          limits:
            cpu: 100m
            memory: 100Mi
          requests:
            cpu: 25m
            memory: 25Mi
        securityContext:
          privileged: true
        volumeMounts:
        - mountPath: /run/xtables.lock
          name: xtables-lock
        - mountPath: /etc/coredns
          name: config-volume
        - mountPath: /etc/kube-dns
          name: kube-dns-config
      dnsPolicy: Default
      hostNetwork: true
      priorityClassName: system-node-critical
      serviceAccountName: node-local-dns
      tolerations:
      - key: CriticalAddonsOnly
        operator: Exists
      - effect: NoSchedule
        operator: Exists
      - effect: NoSchedule
        operator: Exists
      volumes:
      - hostPath:
          path: /run/xtables.lock
          type: FileOrCreate
        name: xtables-lock
      - configMap:
          items:
          - key: Corefile
            path: Corefile.base
          name: node-local-dns-` + configMapHash + `
        name: config-volume
      - configMap:
          name: kube-dns
          optional: true
        name: kube-dns-config
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 10
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`
				return out
			}
			vpaYAML = `apiVersion: autoscaling.k8s.io/v1beta2
kind: VerticalPodAutoscaler
metadata:
  creationTimestamp: null
  name: node-local-dns
  namespace: kube-system
spec:
  resourcePolicy:
    containerPolicies:
    - containerName: '*'
      minAllowed:
        cpu: 50m
        memory: 150Mi
  targetRef:
    apiVersion: apps/v1
    kind: DaemonSet
    name: node-local-dns
  updatePolicy:
    updateMode: Auto
status: {}
`
		)

		JustBeforeEach(func() {
			component = New(c, namespace, values)
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(MatchError(apierrors.NewNotFound(schema.GroupResource{Group: resourcesv1alpha1.SchemeGroupVersion.Group, Resource: "managedresources"}, managedResource.Name)))
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(MatchError(apierrors.NewNotFound(schema.GroupResource{Group: corev1.SchemeGroupVersion.Group, Resource: "secrets"}, managedResourceSecret.Name)))

			Expect(component.Deploy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(Succeed())
			Expect(managedResource).To(DeepEqual(&resourcesv1alpha1.ManagedResource{
				TypeMeta: metav1.TypeMeta{
					APIVersion: resourcesv1alpha1.SchemeGroupVersion.String(),
					Kind:       "ManagedResource",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            managedResource.Name,
					Namespace:       managedResource.Namespace,
					ResourceVersion: "1",
					Labels:          map[string]string{"origin": "gardener"},
				},
				Spec: resourcesv1alpha1.ManagedResourceSpec{
					InjectLabels: map[string]string{"shoot.gardener.cloud/no-cleanup": "true"},
					SecretRefs: []corev1.LocalObjectReference{{
						Name: managedResourceSecret.Name,
					}},
					KeepObjects: pointer.BoolPtr(false),
				},
			}))

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(Succeed())
			Expect(managedResourceSecret.Type).To(Equal(corev1.SecretTypeOpaque))
			Expect(string(managedResourceSecret.Data["serviceaccount__kube-system__node-local-dns.yaml"])).To(Equal(serviceAccountYAML))
			Expect(string(managedResourceSecret.Data["clusterrole____gardener.cloud_psp_kube-system_node-local-dns.yaml"])).To(Equal(clusterRoleYAML))
			Expect(string(managedResourceSecret.Data["rolebinding__kube-system__gardener.cloud_psp_node-local-dns.yaml"])).To(Equal(roleBindingYAML))
			Expect(string(managedResourceSecret.Data["podsecuritypolicy____gardener.kube-system.node-local-dns.yaml"])).To(Equal(podSecurityPolicyYAML))
			Expect(string(managedResourceSecret.Data["service__kube-system__kube-dns-upstream.yaml"])).To(Equal(serviceYAML))
		})

		Context("NodeLocalDNS with ipvsEnabled not enabled", func() {
			BeforeEach(func() {
				values.ClusterDNS = "__PILLAR__CLUSTER__DNS__"
				values.DNSServer = "1.2.3.4"
			})
			Context("ConfigMap", func() {
				JustBeforeEach(func() {
					configMapData := map[string]string{
						"Corefile": `cluster.local:53 {
  errors
  cache {
          success 9984 30
          denial 9984 5
  }
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  health ` + nodeLocal + `:8080
  }
in-addr.arpa:53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
.ip6.arpa:53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
.53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . __PILLAR__UPSTREAM__SERVERS__ {
          ` + forceTcpToUpstreamDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
}
`,
					}
					configMapHash = utils.ComputeConfigMapChecksum(configMapData)[:8]
				})
				Context("Case1", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = true
						values.ForceTcpToUpstreamDNS = true
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
				Context("Case2", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = true
						values.ForceTcpToUpstreamDNS = false
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
				Context("Case3", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = false
						values.ForceTcpToUpstreamDNS = true
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
				Context("Case4", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = false
						values.ForceTcpToUpstreamDNS = false
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
			})
		})

		Context("NodeLocalDNS with ipvsEnabled enabled", func() {
			BeforeEach(func() {
				values.ClusterDNS = "1.2.3.4"
				values.DNSServer = ""
			})

			Context("ConfigMap", func() {
				JustBeforeEach(func() {
					configMapData := map[string]string{
						"Corefile": `cluster.local:53 {
  errors
  cache {
          success 9984 30
          denial 9984 5
  }
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  health ` + nodeLocal + `:8080
  }
in-addr.arpa:53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
.ip6.arpa:53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . ` + values.ClusterDNS + ` {
          ` + forceTcpToClusterDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
.53 {
  errors
  cache 30
  reload
  loop
  bind ` + nodeLocal + values.DNSServer + `
  forward . __PILLAR__UPSTREAM__SERVERS__ {
          ` + forceTcpToUpstreamDNS(values) + `
  }
  prometheus :` + strconv.Itoa(prometheusPort) + `
  }
}
`,
					}
					configMapHash = utils.ComputeConfigMapChecksum(configMapData)[:8]
				})
				Context("Case1", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = true
						values.ForceTcpToUpstreamDNS = true
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

				})
				Context("Case2", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = true
						values.ForceTcpToUpstreamDNS = false
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
				Context("Case3", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = false
						values.ForceTcpToUpstreamDNS = true
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
				Context("Case4", func() {
					BeforeEach(func() {
						values.ForceTcpToClusterDNS = false
						values.ForceTcpToUpstreamDNS = false
					})
					Context("w/o VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = false
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})

					Context("w/ VPA", func() {
						BeforeEach(func() {
							values.VPAEnabled = true
						})

						It("should succesfully deploy all resources", func() {
							Expect(string(managedResourceSecret.Data["configmap__kube-system__node-local-dns-"+configMapHash+".yaml"])).To(Equal(configMapYAML()))
							Expect(string(managedResourceSecret.Data["verticalpodautoscaler__kube-system__node-local-dns.yaml"])).To(Equal(vpaYAML))
							Expect(string(managedResourceSecret.Data["daemonset__kube-system__node-local-dns.yaml"])).To(Equal(daemonsetYAMLFor()))
						})
					})
				})
			})
		})
	})

	Describe("#Destroy", func() {
		It("should successfully destroy all resources", func() {
			Expect(c.Create(ctx, managedResource)).To(Succeed())
			Expect(c.Create(ctx, managedResourceSecret)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(Succeed())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(Succeed())

			Expect(component.Destroy(ctx)).To(Succeed())

			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResource), managedResource)).To(MatchError(apierrors.NewNotFound(schema.GroupResource{Group: resourcesv1alpha1.SchemeGroupVersion.Group, Resource: "managedresources"}, managedResource.Name)))
			Expect(c.Get(ctx, client.ObjectKeyFromObject(managedResourceSecret), managedResourceSecret)).To(MatchError(apierrors.NewNotFound(schema.GroupResource{Group: corev1.SchemeGroupVersion.Group, Resource: "secrets"}, managedResourceSecret.Name)))
		})
	})

	Context("waiting functions", func() {
		var (
			fakeOps   *retryfake.Ops
			resetVars func()
		)

		BeforeEach(func() {
			fakeOps = &retryfake.Ops{MaxAttempts: 1}
			resetVars = test.WithVars(
				&retry.Until, fakeOps.Until,
				&retry.UntilTimeout, fakeOps.UntilTimeout,
			)
		})

		AfterEach(func() {
			resetVars()
		})

		Describe("#Wait", func() {
			It("should fail because reading the ManagedResource fails", func() {
				Expect(component.Wait(ctx)).To(MatchError(ContainSubstring("not found")))
			})

			It("should fail because the ManagedResource doesn't become healthy", func() {
				fakeOps.MaxAttempts = 2

				Expect(c.Create(ctx, &resourcesv1alpha1.ManagedResource{
					ObjectMeta: metav1.ObjectMeta{
						Name:       managedResourceName,
						Namespace:  namespace,
						Generation: 1,
					},
					Status: resourcesv1alpha1.ManagedResourceStatus{
						ObservedGeneration: 1,
						Conditions: []gardencorev1beta1.Condition{
							{
								Type:   resourcesv1alpha1.ResourcesApplied,
								Status: gardencorev1beta1.ConditionFalse,
							},
							{
								Type:   resourcesv1alpha1.ResourcesHealthy,
								Status: gardencorev1beta1.ConditionFalse,
							},
						},
					},
				}))

				Expect(component.Wait(ctx)).To(MatchError(ContainSubstring("is not healthy")))
			})

			It("should successfully wait for the managed resource to become healthy", func() {
				fakeOps.MaxAttempts = 2

				Expect(c.Create(ctx, &resourcesv1alpha1.ManagedResource{
					ObjectMeta: metav1.ObjectMeta{
						Name:       managedResourceName,
						Namespace:  namespace,
						Generation: 1,
					},
					Status: resourcesv1alpha1.ManagedResourceStatus{
						ObservedGeneration: 1,
						Conditions: []gardencorev1beta1.Condition{
							{
								Type:   resourcesv1alpha1.ResourcesApplied,
								Status: gardencorev1beta1.ConditionTrue,
							},
							{
								Type:   resourcesv1alpha1.ResourcesHealthy,
								Status: gardencorev1beta1.ConditionTrue,
							},
						},
					},
				}))

				Expect(component.Wait(ctx)).To(Succeed())
			})
		})

		Describe("#WaitCleanup", func() {
			It("should fail when the wait for the managed resource deletion times out", func() {
				fakeOps.MaxAttempts = 2

				Expect(c.Create(ctx, managedResource)).To(Succeed())

				Expect(component.WaitCleanup(ctx)).To(MatchError(ContainSubstring("still exists")))
			})

			It("should not return an error when it's already removed", func() {
				Expect(component.WaitCleanup(ctx)).To(Succeed())
			})
		})
	})

})

func containerArg(values Values) string {
	if values.DNSServer != "" {
		return common.NodeLocalIPVSAddress + "," + values.DNSServer
	} else {
		return common.NodeLocalIPVSAddress
	}
}
func forceTcpToClusterDNS(values Values) string {
	if values.ForceTcpToClusterDNS {
		return "force_tcp"
	} else {
		return "prefer_udp"
	}
}
func forceTcpToUpstreamDNS(values Values) string {
	if values.ForceTcpToUpstreamDNS {
		return "force_tcp"
	} else {
		return "prefer_udp"
	}
}
