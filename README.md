# DO Kubernetes Network Monitoring Suite

## Use
1) Install the [Kubernetes Prometheus Monitoring Stack from Marketplace](https://marketplace.digitalocean.com/apps/kubernetes-monitoring-stack). `doctl k8s 1-click install <CLUSTER_UUID> --1-clicks=monitoring`.
1) `kubectl apply -f ./manifests/*.yaml`

## Viewing
### Port-forward to Prometheus
```
kubectl port-forward -n kube-prometheus-stack svc/kube-prometheus-stack-prometheus 9090:9090
```

### Grafana
1) Get Password `kubectl get secret --namespace kube-prometheus-stack kube-prometheus-stack-grafana -o jsonpath="{.data.admin-password}" | base64 --decode ; echo`
1) Port Forward `kubectl port-forward -n kube-prometheus-stack svc/kube-prometheus-stack-grafana 8080:80`
1) Login with admin username and password from above
1) First time only: Import https://raw.githubusercontent.com/SuperQ/smokeping_prober/master/dashboard.json as dashboard (+ on top right -> import dashboard)
1) First time only: Import https://grafana.com/grafana/dashboards/1860-node-exporter-full/
1) http://localhost:8080/dashboards


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

### TCP Persistent Prober
Tools like blackbox exporter establish a new TCP/HTTP connection for each probe. That type of monitoring is very useful for packet loss and latency monitoring. Kubernetes TCP service routing, however, features indirections which are not always obvious and depends upon stateful routing and rewriting. This tool aims to simluate long-lived connections leveraging Kubernetes service networking and monitors them for interruptions. Correlating this with data from kube-state-metrics and prometheus-node-exporter will hopefully allow us to diagnose any unexpected interruptions.


#### Building
Building this tool requires [go](https://go.dev) and [ko](https://ko.build).
```
KO_DOCKER_REPO=docker.io/cbaker090/tcp-persistent-prober ko build ./cmd/tcp-persistent-prober --bare --push
```