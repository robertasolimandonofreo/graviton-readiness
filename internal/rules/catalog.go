package rules

// Severity ranks how much a finding matters for moving a workload to Graviton.
type Severity string

const (
	// Blocker means the workload cannot run on an arm64 node as it is.
	Blocker Severity = "blocker"
	// Caveat means it probably can, but something needs a look before the move.
	Caveat Severity = "caveat"
	// Info is advice that does not change the verdict.
	Info Severity = "info"
)

// Rule documents one check. The catalog is the single source for the reports and for explain.
type Rule struct {
	ID       string
	Severity Severity
	Title    string
	// Why says what goes wrong on Graviton.
	Why string
	// Fix says what to change.
	Fix string
	// Example is a short YAML or command that shows the problem and the fix.
	Example string
}

// Catalog lists every rule, in ID order.
var Catalog = []Rule{
	{
		ID: "GR001", Severity: Blocker,
		Title: "Pinned to amd64 by a node selector or required node affinity",
		Why:   "The scheduler never places the pod on an arm64 node, so the workload cannot use Graviton capacity at all, whatever its image supports.",
		Fix:   "Remove the kubernetes.io/arch constraint, or allow both architectures. Do this only after the images are multi-arch (see GR004).",
		Example: `# problem
nodeSelector:
  kubernetes.io/arch: amd64
# fix: allow both (or drop the selector)
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/arch
              operator: In
              values: [amd64, arm64]`,
	},
	{
		ID: "GR002", Severity: Blocker,
		Title: "Pinned to an x86 instance type",
		Why:   "A selector or required affinity on the instance type (node.kubernetes.io/instance-type, or karpenter.k8s.aws/instance-family) that lists only x86 types keeps the pod off Graviton nodes.",
		Fix:   "Select by capability instead (CPU, memory) and let the node pool choose, or add the Graviton equivalents (m7g for m7i, c7g for c7i, r7g for r7i).",
		Example: `# problem
nodeSelector:
  node.kubernetes.io/instance-type: m5.2xlarge
# fix: add Graviton types, or drop the selector and let Karpenter choose
values: [m5.2xlarge, m7g.2xlarge]`,
	},
	{
		ID: "GR003", Severity: Blocker,
		Title: "Image reference names an amd64 build",
		Why:   "A tag such as 1.4-amd64, or a repository such as amd64/ubuntu, is an x86-only image. It starts on an arm64 node and fails with an exec format error.",
		Fix:   "Use the multi-arch tag, the one without the architecture in it, or the arm64 build, and keep the tag identical across architectures so one manifest serves both.",
		Example: `# problem
image: registry.example.com/api:1.4-amd64
# fix
image: registry.example.com/api:1.4   # a manifest list with amd64 and arm64`,
	},
	{
		ID: "GR004", Severity: Blocker,
		Title:   "Image has no linux/arm64 manifest",
		Why:     "The registry has the image, but not for arm64. On an arm64 node the container fails with an exec format error. Needs --check-images.",
		Fix:     "Build and push a multi-arch image (docker buildx build --platform linux/amd64,linux/arm64), or pin the workload to amd64 until it is. In a mixed cluster an unpinned workload can land on an arm64 node.",
		Example: `docker buildx build --platform linux/amd64,linux/arm64 -t registry.example.com/api:1.4 --push .`,
	},
	{
		ID: "GR005", Severity: Info,
		Title:   "Image architecture could not be verified",
		Why:     "The registry did not answer for this image (private registry without credentials, network, a templated reference), so nothing is known about its architectures. Needs --check-images.",
		Fix:     "Log in to the registry (the Docker config and its credential helpers are used) and run again, or inspect it yourself with docker buildx imagetools inspect.",
		Example: `docker buildx imagetools inspect registry.example.com/api:1.4`,
	},
	{
		ID: "GR006", Severity: Caveat,
		Title: "Command, args or environment mention an x86 architecture",
		Why:   "A command that downloads an amd64 binary, or a variable such as ARCH=amd64, keeps working on x86 and breaks on arm64 even when the image is multi-arch.",
		Fix:   "Derive the architecture at run time (uname -m, or TARGETARCH at build time) instead of writing it down.",
		Example: `# problem
args: ["curl -L https://example.com/tool_linux_amd64 -o /bin/tool"]
# fix
args: ["curl -L https://example.com/tool_linux_$(dpkg --print-architecture) -o /bin/tool"]`,
	},
	{
		ID: "GR007", Severity: Caveat,
		Title: "Requests a GPU",
		Why:   "Graviton has few GPU instance types (G5g), and the CUDA stack, drivers and your framework must support arm64. Most GPU workloads are not a Graviton candidate.",
		Fix:   "Check that the GPU instance exists in your region and that the framework publishes arm64 builds, or keep the workload on x86.",
		Example: `resources:
  limits:
    nvidia.com/gpu: 1`,
	},
	{
		ID: "GR008", Severity: Info,
		Title:   "DaemonSet runs on every node",
		Why:     "A DaemonSet is scheduled on each node, including arm64 ones, so one x86-only agent image is enough to break every Graviton node you add.",
		Fix:     "Make sure the DaemonSet image is multi-arch before the first Graviton node joins the cluster.",
		Example: `kind: DaemonSet`,
	},
	{
		ID: "GR009", Severity: Blocker,
		Title: "Targets Windows nodes",
		Why:   "There are no Windows nodes on Graviton.",
		Fix:   "Keep the workload on Windows x86 nodes, or port it to Linux containers.",
		Example: `nodeSelector:
  kubernetes.io/os: windows`,
	},
	{
		ID: "GR010", Severity: Blocker,
		Title: "Selects a NodePool that only provisions amd64",
		Why:   "The pod points at a Karpenter NodePool (by karpenter.sh/nodepool, or by a label that the pool puts on its nodes), and that pool only creates x86 nodes. The pod itself says nothing about the architecture, so the image can be multi-arch and the workload still never reaches a Graviton node. The pool is amd64-only when its requirements say so (kubernetes.io/arch) or list only x86 instance types.",
		Fix:   "Point the workload at a NodePool that can provision arm64, or create one (kubernetes.io/arch In [arm64] with Graviton families such as m7g or c7g), and move the selector. Do it after the images are multi-arch (see GR004).",
		Example: `# problem
nodeSelector:
  karpenter.sh/nodepool: prod-app    # prod-app: kubernetes.io/arch In [amd64]
# fix
nodeSelector:
  karpenter.sh/nodepool: prod-app-arm64`,
	},
	{
		ID: "GR011", Severity: Info,
		Title:   "Selects a NodePool that is not among the scanned manifests",
		Why:     "The pod selects a pool by name or by label, and no NodePool with that name or label was found in the scanned files, so its architecture is unknown.",
		Fix:     "Scan the folder that holds your NodePool manifests too, for example scan ./k8s ./karpenter.",
		Example: `graviton-readiness scan ./k8s ./karpenter`,
	},
}

// Lookup finds a rule by ID.
func Lookup(id string) (Rule, bool) {
	for _, r := range Catalog {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}
