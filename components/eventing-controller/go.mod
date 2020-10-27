module github.com/kyma-project/kyma/components/eventing-controller

go 1.13

require (
	github.com/cloudevents/sdk-go/v2 v2.3.1
	github.com/go-logr/logr v0.1.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/kyma-incubator/api-gateway v0.0.0-20200930072023-5d3f2107a1ef
	github.com/kyma-project/kyma/components/event-publisher-proxy v0.0.0-20201014135541-82b304ab245a
	github.com/mitchellh/hashstructure v1.0.0
	github.com/onsi/ginkgo v1.14.0
	github.com/onsi/gomega v1.10.2
	github.com/ory/oathkeeper-maester v0.1.0
	github.com/pkg/errors v0.9.1
	go.opencensus.io v0.22.4 // indirect
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d
	k8s.io/api v0.18.2
	k8s.io/apimachinery v0.18.2
	k8s.io/client-go v0.18.2
	k8s.io/kubernetes v1.18.2
	sigs.k8s.io/controller-runtime v0.6.0
)

// Replacing each pakage as mentioned https://github.com/kubernetes/kubernetes/issues/79384#issuecomment-684796725
replace k8s.io/api => k8s.io/api v0.18.2

replace k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.18.2

replace k8s.io/apimachinery => k8s.io/apimachinery v0.18.3-beta.0

replace k8s.io/apiserver => k8s.io/apiserver v0.18.2

replace k8s.io/cli-runtime => k8s.io/cli-runtime v0.18.2

replace k8s.io/client-go => k8s.io/client-go v0.18.2

replace k8s.io/cloud-provider => k8s.io/cloud-provider v0.18.2

replace k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.18.2

replace k8s.io/code-generator => k8s.io/code-generator v0.18.3-beta.0

replace k8s.io/component-base => k8s.io/component-base v0.18.2

replace k8s.io/cri-api => k8s.io/cri-api v0.18.11-rc.0

replace k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.18.2

replace k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.18.2

replace k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.18.2

replace k8s.io/kube-proxy => k8s.io/kube-proxy v0.18.2

replace k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.18.2

replace k8s.io/kubectl => k8s.io/kubectl v0.18.2

replace k8s.io/kubelet => k8s.io/kubelet v0.18.2

replace k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.18.2

replace k8s.io/metrics => k8s.io/metrics v0.18.2

replace k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.18.2

replace k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.18.2

replace k8s.io/sample-controller => k8s.io/sample-controller v0.18.2
