# Phrontis

Phrontis shows what a Kubernetes cluster costs per namespace and workload, how much of it sits idle, and which container requests to change.
It is read-only: it only lists and reads cluster objects (nodes, pods, persistent volumes, services, limit ranges, replicasets, statefulsets, daemonsets, jobs, pod disruption budgets, horizontal pod autoscalers, storage classes and pod metrics). Of namespaces it reads only `kube-system`, to get the cluster UID. It does not change anything in your cluster.

## Install

```bash
git clone https://github.com/cursopic/phrontis-agent && cd phrontis-agent
go build -o phrontis ./cmd/phrontis
```

Or build release binaries for darwin and linux with `scripts/release-build.sh <version>`.

## Use

```bash
phrontis --context my-cluster          # cost report from the live cluster
phrontis --context my-cluster --json   # the same report as JSON
phrontis --help                        # all flags
```

The CLI reads the cluster through your kubeconfig. Node prices come from public AWS and Azure list prices; switch that off with `--cloud-prices=false` to use flat `--cpu-hour` and `--gib-hour` prices.

`phrontis mcp` starts a read-only MCP server, so an AI assistant can ask for the same report.

## Agent (Helm)

The agent runs in your cluster and sends a sample every `every` (default 60s) to the Phrontis API URL you give it.

```bash
helm install phrontis-agent deploy/helm/phrontis-agent \
  --namespace phrontis --create-namespace \
  --set api.url=<your Phrontis API URL> \
  --set api.existingSecret=<secret that holds the cluster key>
```

The chart's ClusterRole only has `get`, `list` and `watch`. The agent reads no Secrets, ConfigMaps or logs. It sends nothing except its samples to the API URL you set.

## License

Apache-2.0. See `LICENSE` and `NOTICE`.
