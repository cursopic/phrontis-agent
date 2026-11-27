# What the agent can access

The agent is read-only. Its ClusterRole gives it get, list, watch and nothing else.
It cannot create, change or delete anything, and it cannot read Secrets, ConfigMaps or pod logs.

This page is generated from the Helm chart by `phrontis permissions --md`. A test fails if they differ.

| API group | Resources | Why |
|---|---|---|
| core | nodes, pods, persistentvolumes, services, limitranges | Node size and price, what each pod requests, volumes and load balancers, and the defaults a namespace sets. |
| core | namespaces (only kube-system) | Only kube-system. Its UID names the cluster, so the API key can be checked against it. |
| apps | replicasets, statefulsets, daemonsets | Find the workload that owns each pod, so cost is shown per workload. |
| policy | poddisruptionbudgets | Flag pods that cannot be evicted safely. |
| autoscaling | horizontalpodautoscalers | Mark recommendations for workloads that an autoscaler already scales. |
| batch | jobs | Find the workload that owns each job pod. |
| storage.k8s.io | storageclasses | Price volumes by storage class. |
| metrics.k8s.io | pods | Measured CPU and memory use, the basis of every recommendation. |
