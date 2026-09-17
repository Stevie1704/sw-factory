package factory

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// resolveWorkerCaches binds every repository-declared cache name to the host
// directory the registration maps it to. A declared cache with no host mapping
// fails closed rather than falling back to a host path a repository commit
// could choose.
func resolveWorkerCaches(caches []config.CacheConfig, registration config.RepositoryRegistration) ([]worker.CacheMount, error) {
	result := make([]worker.CacheMount, 0, len(caches))
	for _, cache := range caches {
		hostPath := strings.TrimSpace(registration.Caches[cache.Name])
		if hostPath == "" {
			return nil, fmt.Errorf("repository cache %q has no host path; map caches.%s under cache_root in the host configuration", cache.Name, cache.Name)
		}
		result = append(result, worker.CacheMount{Name: cache.Name, HostPath: hostPath, ReadOnly: cache.ReadOnly})
	}
	return result, nil
}
