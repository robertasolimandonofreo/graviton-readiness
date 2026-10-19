// Package image finds out which platforms a container image was published for, by reading its
// manifest from the registry. It never pulls layers.
package image

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Status says what the check could tell about an image.
type Status string

const (
	// Arm64 means the image has a linux/arm64 manifest.
	Arm64 Status = "arm64"
	// NoArm64 means the manifest was read and has no linux/arm64 entry.
	NoArm64 Status = "no-arm64"
	// Unknown means the manifest could not be read (private registry, network, not a literal
	// reference): nothing is said about the image.
	Unknown Status = "unknown"
)

// Result is the outcome for one image reference.
type Result struct {
	Status Status
	// Platforms lists os/arch of the manifest, sorted, when it could be read.
	Platforms []string
	// Reason explains an Unknown status.
	Reason string
}

// Checker resolves image references to a Result.
type Checker interface {
	Check(ctx context.Context, ref string) Result
}

// Registry is the Checker that talks to registries, with the credentials of the Docker config
// (and its credential helpers, which is how ECR and GCR work). Results are cached by reference.
type Registry struct {
	// Timeout bounds one lookup. Zero means 20 seconds.
	Timeout time.Duration
	// Options are extra remote options, used by tests to reach a local registry.
	Options []remote.Option

	mu    sync.Mutex
	cache map[string]*entry
}

type entry struct {
	once sync.Once
	res  Result
}

// Check implements Checker. It is safe for concurrent use, and asks the registry once per
// distinct reference.
func (r *Registry) Check(ctx context.Context, ref string) Result {
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]*entry{}
	}
	e, ok := r.cache[ref]
	if !ok {
		e = &entry{}
		r.cache[ref] = e
	}
	r.mu.Unlock()
	e.once.Do(func() { e.res = r.lookup(ctx, ref) })
	return e.res
}

func (r *Registry) lookup(ctx context.Context, ref string) Result {
	if strings.ContainsAny(ref, "${}") {
		return Result{Status: Unknown, Reason: "not a literal image reference"}
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Result{Status: Unknown, Reason: "invalid image reference"}
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opts := append([]remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}, r.Options...)

	desc, err := remote.Get(parsed, opts...)
	if err != nil {
		return Result{Status: Unknown, Reason: shorten(err)}
	}
	var platforms []string
	switch {
	case desc.MediaType.IsIndex():
		idx, err := desc.ImageIndex()
		if err != nil {
			return Result{Status: Unknown, Reason: shorten(err)}
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			return Result{Status: Unknown, Reason: shorten(err)}
		}
		for _, m := range manifest.Manifests {
			if m.Platform != nil {
				platforms = append(platforms, platform(m.Platform))
			}
		}
	case desc.MediaType.IsImage():
		img, err := desc.Image()
		if err != nil {
			return Result{Status: Unknown, Reason: shorten(err)}
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			return Result{Status: Unknown, Reason: shorten(err)}
		}
		platforms = append(platforms, platform(&v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture}))
	default:
		return Result{Status: Unknown, Reason: "unsupported manifest type " + string(desc.MediaType)}
	}
	return classify(platforms)
}

// platform renders os/arch, ignoring the variant: arm64/v8 is arm64.
func platform(p *v1.Platform) string { return p.OS + "/" + p.Architecture }

// classify turns a platform list into a Result. Attestation entries (unknown/unknown, which
// BuildKit adds next to the real images) are not platforms and are dropped.
func classify(platforms []string) Result {
	seen := map[string]bool{}
	var list []string
	for _, p := range platforms {
		if p == "unknown/unknown" || seen[p] {
			continue
		}
		seen[p] = true
		list = append(list, p)
	}
	sort.Strings(list)
	if seen["linux/arm64"] {
		return Result{Status: Arm64, Platforms: list}
	}
	if len(list) == 0 {
		return Result{Status: Unknown, Reason: "the manifest lists no platform"}
	}
	return Result{Status: NoArm64, Platforms: list}
}

// shorten keeps the first line of a registry error, which is the readable part.
func shorten(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 160 {
		msg = msg[:160] + "..."
	}
	return msg
}

// CheckAll resolves every distinct reference with at most concurrency lookups at a time.
func CheckAll(ctx context.Context, c Checker, refs []string, concurrency int) map[string]Result {
	if concurrency < 1 {
		concurrency = 1
	}
	out := make(map[string]Result, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := c.Check(ctx, ref)
			mu.Lock()
			out[ref] = res
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// Describe is a one-line summary for reports.
func (r Result) Describe() string {
	switch r.Status {
	case Arm64:
		return "arm64 available"
	case NoArm64:
		return "no linux/arm64 (has " + strings.Join(r.Platforms, ", ") + ")"
	}
	if r.Reason != "" {
		return fmt.Sprintf("not verified: %s", r.Reason)
	}
	return "not verified"
}
