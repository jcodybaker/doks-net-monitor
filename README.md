# DO Kubernetes Network Monitoring Suite

## Use
1) Install the [Kubernetes Prometheus Monitoring Stack from Marketplace](https://marketplace.digitalocean.com/apps/kubernetes-monitoring-stack). `doctl k8s 1-click install <CLUSTER_UUID> --1-clicks=monitoring`.
1) `kubectl apply -f ./manifests/*.yaml`

## Tools

### Smokeping_Prober
[smokeping_prober](https://github.com/SuperQ/smokeping_prober) - This tool provides high-frequency ICMP/UDP monitoring of a defined list of services and exports results in the standard prometheus format.

### Node Templator
This small tool watches Node resources on Kubernetes and on changes renders a templated config and sends a HUP signal to a named process.  This is a companion tool to [smokeping_prober](https://github.com/SuperQ/smokeping_prober) to enable dynamic endpoint discovery.

#### Building
Building this tool requires [go](https://go.dev) and [ko](https://ko.build).
```
KO_DOCKER_REPO=docker.io/cbaker090/k8s-node-templater ko build ./cmd/node-templator --bare --push
```