# Sandboxes on Kubernetes

What a sandbox on the Kubernetes environment can do, how to reach a server
running inside one, what it can reach itself, and what the cluster has to
allow for it. [Install](install.md) is how to get an installation; this page
is what it then offers.

## What a sandbox gets

Each sandbox is one PersistentVolumeClaim, its workspace, and one Pod in the
namespace the control plane is configured for. Stopping a sandbox deletes
the Pod and keeps the claim, so the files survive a stop and a start.

| Capability | On Kubernetes |
|---|---|
| Commands (`exec`) | yes, with stdin and a terminal when asked |
| Files and archives | yes; while the sandbox is stopped, through a short-lived helper Pod that mounts the claim |
| Warm pools | yes |
| Mesh | yes: members of one mesh reach each other by name and nothing else reaches them |
| Ports: the listing, the proxy, the dial socket, `cella port-forward` | yes, for every port the manifest declares |
| Desktop, screenshots and input | only when the control plane is given a display image; without one, a manifest that asks for a desktop is refused when it is resolved |
| Interactive terminal (`attach`, `cella exec -i` and `-t`) | yes, with the window's size carried through and the shell's exit code returned |
| Egress boundaries: `none`, `allowlist`, `open` | yes, once the control plane is told which Pods are the egress gateway; without that, a boundary is recorded and not enforced |
| Resize, volumes, snapshots | no |

A capability the environment does not provide is refused before anything
runs: the route answers `422` with `capability_unsupported`, and a manifest
that needs one is refused when it is created.

## Reaching a port

Declare the port by name in the manifest. Only a declared port is
reachable from outside the sandbox:

```json
"spec": {
  "image": "docker.io/library/python:3.13-alpine",
  "command": ["python3", "-m", "http.server", "3000"],
  "network": {"ports": [{"name": "web", "port": 3000}]}
}
```

`GET /v1/sandboxes/demo/ports` lists each declared port as `listening` or
`closed`, read from inside the sandbox when you ask; it needs the right to
read the sandbox. Each of these reaches the port, and needs the right to
run commands in it (`sandbox.exec`), because a connection to a port inside
is as much access as a command:

- `/v1/sandboxes/demo/ports/web/` forwards any HTTP request to the port,
  WebSocket upgrades included, and answers `502` with
  `upstream_unavailable` while nothing listens or the sandbox is not
  running.
- `cella port-forward demo 8080:3000` carries `127.0.0.1:8080` on your
  machine to the port, one connection to the control plane per connection
  you open.
- The dial socket, `GET /v1/sandboxes/demo/dial/3000` with the
  `cella.dial.v1` subprotocol, carries raw bytes both ways. A port the
  manifest did not declare opens the socket and closes it at once with
  code `1011` and the reason `not_found`; a declared port nothing listens
  on closes it with `1011` and `upstream_unavailable`.

A server that listens only on `127.0.0.1` inside the sandbox is reached as
well as one on every address: the connection is made from inside the
sandbox's own network, to its loopback.

Each connection passes through the cluster's API server and the node's
kubelet, which is why no rule has to open the sandbox's network to the
control plane. The proxy opens one such connection per request, and
`cella port-forward` one per connection your browser keeps open, so a page
with many files opens fewer of them through `cella port-forward`.

Ports are reached on sandboxes of the control plane's own Kubernetes
environment. On an environment that a [self-hosted worker](workers.md)
serves, the proxy and the dial socket answer `422 capability_unsupported`,
whatever runtime the worker drives.

## What a sandbox can reach

Every sandbox runs under a NetworkPolicy of its own, written before its Pod
starts and removed after its Pod is gone. Nothing inside the cluster opens a
connection to a sandbox, except the other members of its mesh. Commands,
files, logs and ports still work, because the control plane reaches a
sandbox through the API server and the node, not over the Pod network.

Once the control plane knows which Pods are the egress gateway, the same
policy also limits what a sandbox reaches: cluster DNS, the gateway, and
the other members of its own mesh, and nothing else. Every other connection
leaves through the gateway, which admits the hosts the manifest allows:

| Boundary | What leaves |
|---|---|
| `open` | every host except those in `deniedHosts` |
| `allowlist` | the hosts in `allowedHosts` and the hosts of every mounted secret |
| `none` | nothing |

The sandbox is pointed at the gateway through its environment:
`HTTPS_PROXY`, `HTTP_PROXY` and their lowercase forms carry the gateway's
address and the sandbox's own credential, `CELLA_GATEWAY_URL` and
`CELLA_GATEWAY_CREDENTIAL` name the gateway's second door for tools that
ignore proxy variables, and the trust variables name
`/run/cella/egress-ca.pem`, the authority the gateway presents when it
substitutes a secret. A program that honors none of them reaches nothing.
`status.conditions` reports `EgressEnforced` true once the gateway holds
the sandbox's boundary.

Mesh members reach each other directly, not through the gateway, at
`<name>.<mesh>`, where `<mesh>` is `mesh-` followed by the lowercase mesh
id after its prefix.

Two limits hold on this runtime:

- A sandbox does not reach the control plane from inside. The control plane
  is outside the policy, so `cella` inside a sandbox cannot call the API; a
  child sandbox is created with the sandbox's token from outside it.
- A sandbox taken from a warm pool started before it had a gateway, so its
  environment carries none of the variables above until its next start,
  whose Pod carries them. Until then its commands reach the gateway only
  where they are pointed at it by hand.

### Turning it on

1. Run the gateway, `cellad egress`, as a Deployment of its own, as the
   [deploy manifests](../deploy/README.md#the-gateway) describe.
2. Point the sandboxes at it: `CELLA_GATEWAY`, and `CELLA_GATEWAY_REVERSE`
   for the second door, in the control plane's ConfigMap.
3. Name its Pods: `CELLA_K8S_GATEWAY_SELECTOR`, the labels the gateway's
   Pods carry, with `CELLA_K8S_GATEWAY_NAMESPACE` where they run in another
   namespace and `CELLA_K8S_GATEWAY_PORTS` where they listen on other
   ports than `3128` and `8080`. A cluster whose DNS Pods are not
   `k8s-app=kube-dns` in `kube-system` sets `CELLA_K8S_DNS_SELECTOR` and
   `CELLA_K8S_DNS_NAMESPACE`. [Configuration](configuration.md#kubernetes)
   has every variable.

Name the Pods only once the gateway runs and carries those labels. A
selector that matches no running Pod leaves every sandbox reaching DNS and
nothing else.

The cluster's network plugin has to enforce NetworkPolicy, egress
included. A plugin that ignores it leaves the policy written and nothing
confined, and the environment still reports the boundary enforced. In a
namespace whose own policies already deny everything but the gateway and
DNS, the per-sandbox policy opens one more path and no other: a mesh
member's connections to the other members of its mesh.

A sandbox created before the gateway was named takes the new policy at its
next start.

## What the cluster has to allow

The control plane's ServiceAccount needs one Role in the sandbox namespace,
which the [deploy manifests](../deploy/README.md#the-role) carry. Commands
and terminals need `get` and `create` on `pods/exec`, and reaching a port
needs the same two on `pods/portforward`: `get` for the WebSocket session
current API servers offer, and `create` for the SPDY upgrade older ones
answer instead.

Reaching a port involves no NetworkPolicy rule. The control plane talks
only to the API server, on the port its own egress rule already admits, and
the kubelet opens the connection inside the sandbox's Pod, where no policy
between Pods applies. Neither the sandbox's own policy nor its mesh's
stands in the way.

The policies themselves are the driver's: one per sandbox and one per mesh,
each made with `create` and replaced or removed with `delete` on
`networkpolicies`, which the same Role grants.

`cellad serve` checks every access in the Role when it starts and does not
start without all of them, naming the ones it is missing; `cellad check`
answers the same question at any time. An installation whose Role was
written for an earlier release adds `get` on `pods/exec` and the
`pods/portforward` rule before it moves to this one. A worker that runs this runtime checks the same list
when it starts.
