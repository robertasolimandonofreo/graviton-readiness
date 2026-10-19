package image

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func newRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func mustRef(t *testing.T, ref string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func imageFor(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: arch})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func pushIndex(t *testing.T, ref string, archs ...string) {
	t.Helper()
	var idx v1.ImageIndex = empty.Index
	for _, arch := range archs {
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        imageFor(t, arch),
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	if err := remote.WriteIndex(mustRef(t, ref), idx); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCheck(t *testing.T) {
	host := newRegistry(t)
	pushIndex(t, host+"/app:multi", "amd64", "arm64")
	pushIndex(t, host+"/app:x86", "amd64")
	if err := remote.Write(mustRef(t, host+"/app:single-arm"), imageFor(t, "arm64")); err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(mustRef(t, host+"/app:single-x86"), imageFor(t, "amd64")); err != nil {
		t.Fatal(err)
	}

	r := &Registry{}
	tests := []struct {
		ref    string
		status Status
	}{
		{host + "/app:multi", Arm64},
		{host + "/app:x86", NoArm64},
		{host + "/app:single-arm", Arm64},
		{host + "/app:single-x86", NoArm64},
		{host + "/app:missing", Unknown},
		{"${REGISTRY}/app:1", Unknown},
		{"not a ref!", Unknown},
	}
	for _, tt := range tests {
		got := r.Check(context.Background(), tt.ref)
		if got.Status != tt.status {
			t.Errorf("%s: status = %s (%s), want %s", tt.ref, got.Status, got.Reason, tt.status)
		}
	}
	if got := r.Check(context.Background(), host+"/app:multi"); strings.Join(got.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Errorf("platforms = %v", got.Platforms)
	}
}

func TestClassifyDropsAttestations(t *testing.T) {
	res := classify([]string{"linux/amd64", "unknown/unknown", "linux/amd64"})
	if res.Status != NoArm64 || len(res.Platforms) != 1 {
		t.Errorf("got %+v", res)
	}
	if res := classify([]string{"unknown/unknown"}); res.Status != Unknown {
		t.Errorf("only attestations should be unknown, got %+v", res)
	}
}

type countingChecker struct{ calls map[string]int }

func (c *countingChecker) Check(_ context.Context, ref string) Result {
	c.calls[ref]++
	return Result{Status: Arm64}
}

func TestCheckAllAsksOncePerReference(t *testing.T) {
	c := &countingChecker{calls: map[string]int{}}
	out := CheckAll(context.Background(), c, []string{"a", "b", "a", "a"}, 1)
	if len(out) != 2 || c.calls["a"] != 1 || c.calls["b"] != 1 {
		t.Errorf("out=%v calls=%v", out, c.calls)
	}
}
