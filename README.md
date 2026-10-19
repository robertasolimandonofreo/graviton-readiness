# graviton-readiness

Graviton readiness check for Kubernetes. It tells you which workloads can move to AWS Graviton
(arm64) nodes, and what blocks the ones that cannot.

"Move to Graviton" is common advice. Finding out which
workloads can actually run on arm64 is usually a guess, and the wrong guess is a pod that crash
loops with `exec format error`. `graviton-readiness` reads your Deployments, StatefulSets,
DaemonSets, Jobs, CronJobs and Pods and checks the two things that decide it: whether the
scheduling configuration lets the pod reach an arm64 node, and whether every image was published
for `linux/arm64`.

> **Status: early (v0.1.0).** It scans YAML/JSON manifests, and can read the platforms of the
> images from their registries. It is a static analysis, not a guarantee: see [Limits](#limits).

## Install

Download a binary for Linux, macOS or Windows (amd64 and arm64) from the
[releases page](https://github.com/robertasolimandonofreo/graviton-readiness/releases), and check it
against `checksums.txt`:

```bash
tar -xzf graviton-readiness_<version>_linux_amd64.tar.gz
sha256sum -c --ignore-missing checksums.txt
sudo mv graviton-readiness /usr/local/bin/
```

Or use the container image (linux/amd64 and linux/arm64):

```bash
docker run --rm -v "$PWD:/work:ro" ghcr.io/robertasolimandonofreo/graviton-readiness:latest scan /work
```

The image only reads the files you mount, and has no Docker config, so `--check-images` only
reaches public registries from it. For private registries use the binary.

Or build it with Go:

```bash
go install github.com/robertasolimandonofreo/graviton-readiness/cmd/graviton-readiness@latest
```

## Use

```bash
graviton-readiness scan ./k8s                          # a directory of manifests
graviton-readiness scan -v deployment.yaml             # explain every finding
helm template my-release ./chart | graviton-readiness scan -
graviton-readiness scan --check-images ./k8s           # also read the image platforms
graviton-readiness scan -f json ./k8s                  # machine-readable
graviton-readiness scan -f markdown ./k8s              # for a pull request comment
graviton-readiness scan --check-images --fail-on not-ready ./k8s   # exit 1 in CI
graviton-readiness explain GR004                       # what a rule means and how to fix it
```

Example output, from `testdata/sample.yaml` (no network):

```
VERDICT     SCORE  WORKLOAD                         REPLICAS  CPU    FINDINGS
unverified  100    kube-system/daemonset/log-agent  -         -      GR008
unverified  100    shop/cronjob/report              -         -      -
not-ready   60     shop/deployment/api              2         1000m  GR001
unverified  100    shop/deployment/web              3         500m   -
not-ready   50     shop/deployment/worker           2         250m   GR003,GR006

5 workloads: 0 ready, 0 with caveats, 2 not ready, 3 unverified
Graviton potential: 0% of requested CPU (workloads ready or with caveats).
Unverified: 37% of requested CPU could move if the images support arm64.
Images were not checked: run with --check-images to read their platforms from the registry.
```

With `--check-images`, a workload whose images support arm64 becomes `ready`.

## Images

Without `--check-images` the tool does not touch the network, and no workload is called `ready`:
a clean configuration is `unverified`, because nothing says its images run on arm64.

With `--check-images` it reads the manifest of every image from its registry (never the layers)
and looks for a `linux/arm64` entry. It uses the credentials of your Docker config and its
credential helpers, which is how ECR, GCR and private registries work, and asks once per distinct
image. If a registry does not answer (no credentials, network, a templated reference), the image is
reported as not verified (GR005) and the workload stays `unverified`: the tool never guesses.

## Verdicts

| Verdict              | Meaning                                                                          |
| -------------------- | -------------------------------------------------------------------------------- |
| `ready`              | No finding blocks it and every image is known to support arm64                   |
| `ready-with-caveats` | The same, with something to look at first                                        |
| `not-ready`          | At least one blocker                                                             |
| `unverified`         | Nothing blocks it in the configuration, but the images were not or could not be checked |

The score starts at 100 and loses 40 per blocker and 10 per caveat. Like the verdict, it only
counts what the manifests and the registry show.

## Rules

| Rule  | Severity | What it checks                                                                       |
| ----- | -------- | ------------------------------------------------------------------------------------ |
| GR001 | Blocker  | Pinned to amd64 by `kubernetes.io/arch` in a node selector or required node affinity |
| GR002 | Blocker  | Only x86 instance types allowed (instance-type selector, or Karpenter instance family) |
| GR003 | Blocker  | Image reference names an amd64 build (`app:1.4-amd64`, `amd64/ubuntu`)               |
| GR004 | Blocker  | Image has no `linux/arm64` manifest (needs `--check-images`)                         |
| GR005 | Info     | Image architecture could not be verified (needs `--check-images`)                    |
| GR006 | Caveat   | Command, args or environment mention `amd64` or `x86_64`                             |
| GR007 | Caveat   | Requests a GPU                                                                       |
| GR008 | Info     | DaemonSet: runs on every node, so its images must be multi-arch first                |
| GR009 | Blocker  | Targets Windows nodes                                                                |
| GR010 | Blocker  | Selects a Karpenter NodePool that only provisions amd64 (see [NodePools](#nodepools)) |
| GR011 | Info     | Selects a NodePool that is not among the scanned manifests                           |

Init containers are checked like the others. `graviton-readiness explain GR004` documents any rule,
with a problem and a fix.

## NodePools

A pod that picks its nodes by `karpenter.sh/nodepool`, or by a label that its NodePool puts on the
nodes (`role: web`), has the architecture of that pool, and the pod itself says nothing about it.
So the image can be multi-arch and the workload still never reaches a Graviton node. When the
scanned files include Karpenter `NodePool` manifests, GR010 links each workload to the pools it can
land on (through its node selector and its required node affinity) and reports it when none of
them can provision arm64. A pool is amd64-only when its requirements say so
(`kubernetes.io/arch In [amd64]`), or list only x86 instance types or families.

```bash
graviton-readiness scan ./k8s ./karpenter     # the workloads and the NodePools together
```

A pool that states no architecture is **not** assumed to be amd64: the Karpenter documentation does
not promise a default, so the rule stays quiet unless the manifest proves it.

**What if there is no NodePool in the scan?** Then the rule does not run, and the tool says so: the
summary counts the workloads that choose nodes by a label no scanned pool defines ("N workloads
choose nodes by a label that no scanned NodePool defines"). That is the case when the pools are not
Karpenter's (an EKS managed node group, a GKE or AKS node pool) or live outside the folder. Declare
them yourself, one per `--node-pool`, as `key=value:arch` with `amd64`, `arm64` or `any`:

```bash
graviton-readiness scan ./k8s \
  --node-pool eks.amazonaws.com/nodegroup=general:amd64 \
  --node-pool cloud.google.com/gke-nodepool=arm-pool:arm64
```

## What the numbers mean

The summary weighs the verdicts by the CPU the workloads request. The figures cover long-running
workloads (Deployments, StatefulSets, Pods): a DaemonSet has no replica count to weigh by, and a
Job requests CPU only while it runs. *Graviton potential* is the CPU of workloads that are ready or
ready with caveats, with images verified. *Unverified* is reported apart and never added to it.

## Exit codes

`0` success, `1` the `--fail-on` level was reached (`not-ready`, `unverified` or `caveats`, each
including the ones before it), `2` a usage or runtime error.

## JSON

The JSON starts with `schemaVersion` and `toolVersion`, then `imagesChecked`, a `summary` and one
entry per workload with its score, verdict, findings and image statuses.

## Limits

- **Ready is not safe.** The tool reads configuration and manifests. It does not know whether the
  application has native dependencies, JIT or assembly that behave differently on arm64, or
  whether its libraries ship arm64 builds. Test a ready workload on a Graviton node before moving
  production.
- **Only your own images and the registry's word.** A manifest with a `linux/arm64` entry means the
  image was published for arm64, not that it works.
- **Performance and cost are out of scope.** It does not measure whether Graviton is cheaper for
  your workload, only whether it can run there.
- Manifests only: it does not read a live cluster or Helm values yet. Render charts with
  `helm template` and pipe them in. Only Karpenter NodePools are read from manifests: other pools
  are declared with `--node-pool`. EC2NodeClass is not read, so an AMI that decides the
  architecture is not seen.
- Rules are heuristics. GR006 looks for the text `amd64` or `x86_64`, so it can miss a hidden
  architecture or flag a harmless mention.
- AWS first: the instance-type rules know Graviton families (m7g, c6gn, x2gd, im4gn, is4gen, g5g,
  hpc7g, a1). Ampere on Azure and Axion on Google Cloud are not recognized by GR002.

## Roadmap

Reading a live cluster (read-only), reading EC2NodeClasses, a GitHub Action with a pull request
comment, SARIF, and a migration plan that groups what to move now and after changes. None of it is
started.

## Related

[spot-readiness](https://github.com/robertasolimandonofreo/spot-readiness) checks which workloads
tolerate Spot interruptions, with the same input and the same kind of report.

## Development

```bash
go test -race ./...
go run ./cmd/graviton-readiness scan testdata
```

The image checks are tested against an in-memory registry, so the tests need no network.

## License

Apache-2.0.
