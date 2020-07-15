apiVersion: "installer.kyma-project.io/v1alpha1"
kind: Installation
metadata:
  name: kyma-installation
  namespace: default
  labels:
    action: install
    kyma-project.io/installation: ""
  finalizers:
    - finalizer.installer.kyma-project.io
spec:
  version: "__VERSION__"
  url: "__URL__"
  components:
    - name: "cluster-essentials"
      namespace: "kyma-system"
    - name: "testing"
      namespace: "kyma-system"
    - name: "cert-manager"
      namespace: "cert-manager"
    - name: "istio"
      namespace: "istio-system"
    - name: "ingress-dns-cert"
      namespace: "istio-system"
    - name: "istio-kyma-patch"
      namespace: "istio-system"
    - name: "dex"
      namespace: "kyma-system"
    - name: "ory"
      namespace: "kyma-system"
    - name: "api-gateway"
      namespace: "kyma-system"
    - name: "core"
      namespace: "kyma-system"
    - name: "console"
      namespace: "kyma-system"
    - name: "cluster-users"
      namespace: "kyma-system"
    - name: "apiserver-proxy"
      namespace: "kyma-system"
    - name: "serverless"
      namespace: "kyma-system"
    - name: "application-connector"
      namespace: "kyma-integration"
    - name: "rafter"
      namespace: "kyma-system"
    - name: "service-catalog"
      namespace: "kyma-system"
    - name: "service-catalog-addons"
      namespace: "kyma-system"
    - name: "knative-serving"
      namespace: "knative-serving"
    - name: "knative-eventing"
      namespace: "knative-eventing"
    - name: "nats-streaming"
      namespace: "natss"
    - name: "knative-provisioner-natss"
      namespace: "knative-eventing"
    - name: "event-sources"
      namespace: "kyma-system"
