# [Gardener Extension for F5 BIG-IP Load Balancing](https://gardener.cloud)

[![REUSE status](https://api.reuse.software/badge/github.com/gardener/gardener-extension-f5)](https://api.reuse.software/info/github.com/gardener/gardener-extension-f5)

Project Gardener implements the automated management and operation of [Kubernetes](https://kubernetes.io/) clusters as a service.

This extension integrates F5 BIG-IP load balancing (via F5 CIS and/or a provider LBaaS API) with Gardener shoot clusters.

## What this extension does

- Registers the Gardener extension type `f5-loadbalancer` (and legacy `f5`).
- Installs a Seed-side controller via Gardener `ControllerRegistration` / `ControllerDeployment` / `ControllerInstallation`.
- Optionally deploys F5 CIS into Shoots for application-plane load balancing.
- Optionally deploys a small “Service type=LoadBalancer bridge” into Shoots to translate Services into CIS-managed Ingress objects.

## Prerequisites

- A Gardener management cluster (“Garden”) and at least one Seed.
- A controller image published to a registry reachable by the Seed cluster.
- `kubectl` contexts for the Garden cluster (and optionally the Seed cluster for verification).
- `helm` v3 (needed to generate the embedded chart in the `ControllerDeployment`).

For production-like setups (for example, provider-openstack):

- BIG-IP must be able to reach Shoot backends (node IPs/NodePorts or pod IPs, depending on mode).
- Firewall / Security Groups must allow the chosen data-plane path.

## Install into a Gardener landscape

The operator runbook lives in deploy/garden/README.md.

It covers:
- build/publish image
- generate `ControllerDeployment`
- apply `ControllerRegistration` / `ControllerDeployment`
- install into a Seed via `ControllerInstallation`

## Configure a Shoot (extension resource)

Example extension resource:

```yaml
apiVersion: extensions.gardener.cloud/v1alpha1
kind: Extension
metadata:
  name: "extension-f5"
  namespace: shoot--project--abc
spec:
  # AirtelCloud expects type f5-loadbalancer; the controller also supports legacy type f5.
  type: f5-loadbalancer
  # Optional: extension-specific configuration (mapped to F5LoadBalancerConfig.spec)
  providerConfig:
    spec:
      enableApplicationLB: false

To enable application-plane LB (deploy CIS into the Shoot), set `enableApplicationLB: true` and configure `F5LoadBalancerConfig` for the Shoot (BIG-IP URL, partition, and credentials secret ref).
```

## Documentation

Please find further documentation under docs/.

Key entry points:

- deploy/garden/README.md (install/register the extension)
- docs/usage/usage.md (config CRD fields and examples)
- docs/demo-a-z.md (copy/paste demo script)

## Local Development

You can run the controller locally on your machine by executing `make start`. Please make sure to have the kubeconfig to the cluster you want to connect to ready in the `./dev/kubeconfig` file.

```bash
make start
```

Static code checks and tests can be executed by running:

```bash
make verify
```

## Feedback and Support

Feedback and contributions are always welcome. Please report bugs or suggestions as [GitHub issues](https://github.com/gardener/gardener-extension-f5/issues) or join our [Slack channel](https://gardener-cloud.slack.com/).

## Learn More

- [Gardener Project](https://gardener.cloud)
- [Gardener Extensions Documentation](https://github.com/gardener/gardener/tree/master/docs/extensions)
- [GEP-1 on Extensibility](https://github.com/gardener/gardener/blob/master/docs/proposals/01-extensibility.md)
