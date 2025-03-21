# Topology Hinter

Bring back topology key-based service routing to Kubernetes.

## Description

[Topology-aware Routing with topology keys](https://kubernetes.io/docs/concepts/services-networking/service-topology/) is deprecated since Kubernetes 1.22. The successor [Topology Aware Hints](https://kubernetes.io/docs/concepts/services-networking/topology-aware-hints/) poses strict restrictions to ensure load balance. However, for clusters running across multiple zones but running just a few replicas, the auto hinting is impossible by kubernetes' built-in EndpointSlice controller.

Topology Hinter takes over the control of EndpointSlice and set hinting based on topology keys. Therefor, features provided by EndpointSlice will be disabled and number of endpoints is limited to 100.

## Logic

The logic is similar to [Topology-aware Routing with topology keys](https://kubernetes.io/docs/concepts/services-networking/service-topology/) used to be. Service endpoints will match by topology keys until matched. Topology keys can be set in ConfigMap in `config/manager/configmap.yaml`. *Reconfigure requires a restart of the hinter*.

Metrics are introduced to further control routing. When "\*" is at the end of topology keys, metrics can be applied to one upper level above "\*". Point-to-point metrics are configurable and the zones with lowest metrics will be used. For example:

Suppose there're four zones: `eu-central-1`, `us-west-1`, `us-west-2`, `ap-north-1`, region are set to `eu-central `, `us-west `and `ap-north `respectively. Endpoints are available in `eu-central-1`, `us-west-1`.

Config:

```yaml
topologyKeys:
- "topology.kubernetes.io/zone"
- "topology.kubernetes.io/region"
- "*"
defaultMetrics: 1000
metrics:
- from: ap-north
  tos:
  - to: eu-central
    metric: 200
  - to: us-west
    metric: 100
```

Then endpoints in `us-west-1` and `us-west-2` will provide service for `us-west-1 `(zone matched), `us-west-2` (region matched) and `ap-north-1 `(matched at *, routed by metrics), `eu-central-1` will serve itself.

## Getting Started

You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Running on the cluster

1. (Optional) Build and push your image to the location specified by `IMG`:

```sh
make docker-build docker-push IMG=gzmaxsum/topology-hinter:0.1.5
```

2. Deploy the controller to the cluster with the image specified by `IMG`:

```sh
make deploy IMG=gzmaxsum/topology-hinter:0.1.5
```

### Undeploy controller

UnDeploy the controller to the cluster:

```sh
make undeploy
```

### Apply annotation to Service

Set annotation to service to hint:

```
kubectl annotate svc <service-name> maxsum.io/topology-hint=true
kubectl annotate svc <service-name> service.kubernetes.io/topology-aware-hints=auto
```

or set using yaml

```yaml
apiVersion: v1
kind: Service
metadata:
  name: service-name
  annotations:
    maxsum.io/topology-hint: "true"
    service.kubernetes.io/topology-aware-hints: auto
spec:
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  selector:
    app: nginx
```

You should see EndpointSlice created with hinting:

```
kubectl get endpointslice service-name-0 -o yaml

```

```
apiVersion: discovery.k8s.io/v1
endpoints:
- addresses:
  - 10.244.4.208
  conditions:
    ready: true
  hints:
    forZones:
    - name: eu-frankfurt-1
  nodeName: de-fra-linux
  zone: eu-frankfurt-1
- addresses:
  - 10.244.7.20
  conditions:
    ready: true
  hints:
    forZones:
    - name: na-sanjose-1
  nodeName: us-sjc-linux
  zone: na-sanjose-1
- addresses:
  - 10.244.8.206
  conditions:
    ready: true
  hints:
    forZones:
    - name: ea-tokyo-1
    - name: ea-tokyo-2
  nodeName: jp-tko-linux
  zone: ea-tokyo-1
kind: EndpointSlice
metadata:
  labels:
    endpointslice.kubernetes.io/managed-by: topology-hinter.maxsum.io
    kubernetes.io/service-name: nginx
  name: service-name-0
  namespace: default
ports:
- name: ""
  port: 80
  protocol: TCP
```

## Metrics on lowest level

This

## Contributing

### How it works

This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
which provides a reconcile function responsible for synchronizing resources untile the desired state is reached on the cluster.

The controller mirrors Endpoints to EndpointSlice and adds hinting based on topology keys. To disable built-in EndpointSlice controller, the controller deletes EndpointSlices created by built-in controller when Endpoint is created.

### Test It Out

1. Install the CRDs into the cluster:

```sh
make install
```

2. Run your controller (this will run in the foreground, so switch to a new terminal if you want to leave it running):

```sh
make run
```

**NOTE:** You can also run this in one step by running: `make install run`

## License

Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
