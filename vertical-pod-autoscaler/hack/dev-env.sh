#!/usr/bin/env bash
set -euo pipefail

kind create cluster --name vpa --image kindest/node:v1.33.0

kubectl apply -f deploy/vpa-v1-crd-gen.yaml
kubectl apply -f deploy/vpa-rbac.yaml

kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl -n kube-system patch deploy metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

helm repo add vm https://victoriametrics.github.io/helm-charts
helm install vm vm/victoria-metrics-single -n monitoring --create-namespace
helm install vmagent vm/victoria-metrics-agent -n monitoring \
  --set 'remoteWrite[0].url=http://vm-victoria-metrics-single-server:8428/api/v1/write' \
  -f hack/testdata/vmagent-ksm.yaml

# kube-state-metrics: the requests/limits series (kube_pod_container_resource_*) for the
# charts of the web page. vmagent scrapes it with honor_labels, see vmagent-ksm.yaml.
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-state-metrics prometheus-community/kube-state-metrics -n monitoring

kubectl apply -f hack/testdata/hamster.yaml
kubectl apply -f hack/testdata/loadtarget.yaml   # load follows the request rate, see hack/load.sh
