# graviton-readiness

Graviton readiness check for Kubernetes. It tells you which workloads can move to AWS Graviton
(arm64) nodes, and what blocks the ones that cannot.

"Move to Graviton" is common advice. Finding out which
workloads can actually run on arm64 is usually a guess, and the wrong guess is a pod that crash
loops with `exec format error`. `graviton-readiness` reads your Deployments, StatefulSets,
DaemonSets, Jobs, CronJobs and Pods and checks the two things that decide it: whether the
scheduling configuration lets the pod reach an arm64 node, and whether every image was published
for `linux/arm64`.

> **Status: early (v0.3.0).** It scans YAML/JSON manifests or a live cluster (read-only), reads the
> platforms of the images from their registries, and plans the migration. It is a static analysis
> of configuration, not a guarantee: see [Limits](#limits).

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
graviton-readiness scan -f sarif ./k8s > gr.sarif      # for GitHub code scanning
graviton-readiness plan --check-images ./k8s ./karpenter   # what to move now, after changes, or never
graviton-readiness scan --cluster                      # a live cluster, read-only
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

## Migration plan

`graviton-readiness plan` turns the verdicts into an order of work. It takes the same input as
`scan` (paths, `--check-images`, `--node-pool`, `--cluster`) and `-f table`, `json` or `markdown`.
Illustrative output, with made-up numbers:

```
GRAVITON MIGRATION PLAN

Total workloads:          42
Ready now:                9
Ready after a check:      2
Ready after changes:      24
Verify first:             5
Keep on amd64:            2

Requested CPU:            38.0 vCPU
Movable now:              6.5 vCPU (17%)
Potential after changes:  31.0 vCPU (81%)
To verify first:          3.0 vCPU (7%), not counted in the potential

Phase 1: Ready now
  ...
Phase 2: Ready after changes
  1. shop/deployment/api (8.0 vCPU)
       GR010  Point the workload at a NodePool that can provision arm64, or create one.
       GR004  Build and push a multi-arch image, or pin the workload to amd64 until it is.
```

- **Ready now:** no blocker, no caveat, and every image runs on arm64. The safest first move, and a
  good pilot.
- **Ready after a check:** ready, with a caveat a person has to look at (an architecture written in
  a command, a GPU).
- **Ready after changes:** blockers that a change in the manifest or in the image build removes. They
  are ordered by the CPU that would move divided by the number of changes, so a big workload that
  needs one change goes before a small one that needs four. It is a way to order the work, not a
  forecast of savings.
- **Verify first:** nothing blocks it in the configuration, but its images were not checked or the
  registry did not answer. Never counted in the potential.
- **Keep on amd64:** needs an architecture that Graviton does not have (Windows). No change fixes it.

## SARIF and code scanning

`-f sarif` writes a SARIF 2.1.0 log for GitHub code scanning. Each finding points at the **line of
the field that caused it**: the `image:` of GR003 to GR005, the node selector of GR001, GR002 and
GR010, the label of a pool. The fingerprint ignores the line, so an alert survives an edit that
moves the manifest. A workload read from a cluster has no file and carries the workload as a
logical location. The output was validated against the official SARIF 2.1.0 schema.

`--json-file`, `--sarif-file` and `--plan-file` write the other reports from the same run, so the
registries are asked once.

## Live cluster

`--cluster` reads the Deployments, StatefulSets, DaemonSets, CronJobs, Argo Rollouts, Karpenter
NodePools and EC2NodeClasses, and the nodes, from the cluster instead of from files.

```bash
graviton-readiness scan --cluster                                  # the current kubeconfig context
graviton-readiness scan --cluster --context prod -n shop           # one context, one namespace
graviton-readiness plan --cluster --check-images
```

It is **read-only**: only `list` calls are ever sent (the tests check that no other verb is used),
paged by 500 objects and limited to a few requests a second, one resource at a time. A resource that
is not installed is skipped with a note. `-n` limits the workloads; NodePools, EC2NodeClasses and
nodes belong to the cluster and are always read whole. Jobs and Pods are not read: almost every one
belongs to a controller that is. The minimum RBAC is `list` on `deployments`, `statefulsets` and
`daemonsets` (apps), `cronjobs` (batch), `rollouts` (argoproj.io), `nodepools` (karpenter.sh),
`ec2nodeclasses` (karpenter.k8s.aws) and `nodes` (core).

**Pools that are not Karpenter's need no `--node-pool` in this mode.** The labels of the nodes give
their architecture: an EKS managed node group, a GKE or AKS node pool is resolved from what its nodes
report, and the message says it was observed (`observed on the cluster: all 2 node(s) labeled
eks.amazonaws.com/nodegroup=core are amd64`). It is a statement about the nodes that exist today: a
pool scaled to zero says nothing, and a pool with both architectures does not block. The same works
for a file made with `kubectl get nodes -o yaml`.

## GitHub Action

The repository is also a GitHub Action: it installs the release that matches its version (checking
the checksum), scans, adds the report to the job summary and, optionally, comments on the pull
request, uploads the SARIF and fails the job.

```yaml
name: graviton-readiness
on: pull_request
permissions:
  contents: read
  pull-requests: write      # to comment
  security-events: write    # to upload the SARIF
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: aws-actions/configure-aws-credentials@v4     # the registry login, if the images are private
        with: {role-to-assume: "arn:aws:iam::123456789012:role/ci-read-ecr", aws-region: us-east-1}
      - uses: aws-actions/amazon-ecr-login@v2
      - uses: robertasolimandonofreo/graviton-readiness@v0.3.0
        with:
          path: ./k8s ./karpenter
          check-images: "true"
          comment: "true"
          comment-plan: "true"
          upload-sarif: "true"
          fail-on: not-ready
```

| Input | Default | |
| --- | --- | --- |
| `path` | `.` | Files or directories to scan, separated by spaces. Put the NodePools in it too |
| `check-images` | `false` | Read the image platforms. The credentials are the ones in the Docker config, so log in first |
| `node-pool` | empty | Pools that are not Karpenter's, one `key=value:arch` per line |
| `fail-on` | empty | `not-ready`, `unverified` or `caveats`. The comment is posted first |
| `comment`, `comment-plan` | `false` | Comment the report, and a second comment with the plan; each is found by its marker and kept updated |
| `upload-sarif` | `false` | Upload the SARIF to code scanning (needs `security-events: write`) |
| `version` | the action's version | A release such as `v0.3.0`; `local` uses the binary on the `PATH` |
| `github-token` | `github.token` | Token used to comment |

Outputs: `potential-percent`, `ready`, `caveats`, `not-ready`, `unverified` and `sarif-file`. It runs
on Linux and macOS runners. One run writes every report, so the registries are asked once. The
comments are found by their markers, so the action never edits the comment of another tool. On pull
requests from forks `GITHUB_TOKEN` is read-only, so `comment` and `upload-sarif` fail there.

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
- No Helm values: render charts with `helm template` and pipe them in. Only Karpenter NodePools are
  read from manifests; with `--cluster` the other pools come from the nodes, and from files they are
  declared with `--node-pool`.
- EC2NodeClass is read for what a manifest proves: a Windows AMI family means amd64 only. The
  architecture of a custom AMI (`amiFamily: Custom`, an AMI chosen by id or tags) is in AWS, not in
  the manifest, and is not read.
- Rules are heuristics. GR006 looks for the text `amd64` or `x86_64`, so it can miss a hidden
  architecture or flag a harmless mention.
- AWS first: the instance-type rules know Graviton families (m7g, c6gn, x2gd, im4gn, is4gen, g5g,
  hpc7g, a1). Ampere on Azure and Axion on Google Cloud are not recognized by GR002.

## Roadmap

Reading Helm values (the `values.yaml` that pins `kubernetes.io/arch`), asking AWS for the
architecture of a custom AMI, and comparing with the base branch of a pull request so that only a
new finding fails the build. None of it is started.

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
