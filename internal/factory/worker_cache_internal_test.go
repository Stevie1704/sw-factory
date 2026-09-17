package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestResolveWorkerCachesBindsDeclaredNamesToHostDirectories verifies the
// repository owns the cache name and access mode while the host owns the path.
func TestResolveWorkerCachesBindsDeclaredNamesToHostDirectories(t *testing.T) {
	t.Parallel()

	mounts, err := resolveWorkerCaches(
		[]config.CacheConfig{{Name: "go-build", ReadOnly: false}, {Name: "npm", ReadOnly: true}},
		config.RepositoryRegistration{
			CacheRoot: "/var/lib/factory/caches",
			Caches:    map[string]string{"go-build": "/var/lib/factory/caches/go-build", "npm": "/var/lib/factory/caches/npm"},
		},
	)
	if err != nil {
		t.Fatalf("resolveWorkerCaches() error = %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %#v, want two declared caches", mounts)
	}
	if mounts[0].Name != "go-build" || mounts[0].HostPath != "/var/lib/factory/caches/go-build" || mounts[0].ReadOnly {
		t.Fatalf("mounts[0] = %#v, want the writable host-mapped go-build cache", mounts[0])
	}
	if mounts[1].Name != "npm" || mounts[1].HostPath != "/var/lib/factory/caches/npm" || !mounts[1].ReadOnly {
		t.Fatalf("mounts[1] = %#v, want the read-only host-mapped npm cache", mounts[1])
	}
}

// TestResolveWorkerCachesFailsClosedOnAnUnmappedCache verifies a repository
// commit that adds a cache name after registration cannot start a worker with
// a defaulted host path.
func TestResolveWorkerCachesFailsClosedOnAnUnmappedCache(t *testing.T) {
	t.Parallel()

	_, err := resolveWorkerCaches(
		[]config.CacheConfig{{Name: "go-build"}},
		config.RepositoryRegistration{CacheRoot: "/var/lib/factory/caches"},
	)
	if err == nil {
		t.Fatal("resolveWorkerCaches() error = nil, want an unmapped-cache failure")
	}
	if !strings.Contains(err.Error(), "go-build") {
		t.Fatalf("error = %v, want the unmapped cache name", err)
	}
}

// TestResolveWorkerCachesIgnoresARepositoryDeclaredHostPath verifies a frozen
// packet carrying a legacy host path still resolves through host
// configuration.
func TestResolveWorkerCachesIgnoresARepositoryDeclaredHostPath(t *testing.T) {
	t.Parallel()

	mounts, err := resolveWorkerCaches(
		[]config.CacheConfig{{Name: "go-build", Path: "/Users/maintainer/.ssh"}},
		config.RepositoryRegistration{
			CacheRoot: "/var/lib/factory/caches",
			Caches:    map[string]string{"go-build": "/var/lib/factory/caches/go-build"},
		},
	)
	if err != nil {
		t.Fatalf("resolveWorkerCaches() error = %v", err)
	}
	if mounts[0].HostPath != "/var/lib/factory/caches/go-build" {
		t.Fatalf("mounts[0] = %#v, want the host-configured directory", mounts[0])
	}
}
