package process

import (
	"time"

	"github.com/awlx/vllm-dashboard/internal/cache"
	"github.com/awlx/vllm-dashboard/internal/config"
	"github.com/awlx/vllm-dashboard/internal/sample"
)

type identity struct {
	info sample.ServerInfo
	pid  string
}

// Cache keeps process and service data. Process discovery runs pgrep, ps,
// and systemctl, so polls and API requests must not start new processes
// every time.
type Cache struct {
	vllmVersion, kernelVersion string
	identities                 *cache.TTL[identity]
	infos                      *cache.TTL[sample.ServerInfo]
}

// NewCache returns a cache that keeps the model identity for 30 seconds
// and the server info for 10 seconds.
func NewCache(vllmVersion, kernelVersion string) *Cache {
	return &Cache{
		vllmVersion:   vllmVersion,
		kernelVersion: kernelVersion,
		identities:    cache.NewTTL[identity](30 * time.Second),
		infos:         cache.NewTTL[sample.ServerInfo](10 * time.Second),
	}
}

// Identity returns the cached result of Identity.
func (c *Cache) Identity(model config.Model) (sample.ServerInfo, string) {
	id, _ := c.identities.Get(model.Key, func() identity {
		info, pid := Identity(model)
		return identity{info: info, pid: pid}
	})
	return id.info, id.pid
}

// ServerInfo returns the cached result of ServerInfo. The uptime includes
// the time since the value was loaded.
func (c *Cache) ServerInfo(model config.Model) sample.ServerInfo {
	info, at := c.infos.Get(model.Key, func() sample.ServerInfo {
		info, pid := c.Identity(model)
		return serverInfo(model, info, pid, c.vllmVersion, c.kernelVersion)
	})
	if info.ServiceUptimeSeconds > 0 {
		info.ServiceUptimeSeconds += int64(time.Since(at) / time.Second)
	}
	return info
}

// Invalidate removes the cached values of a model, for example when its
// engine comes back with a new process.
func (c *Cache) Invalidate(key string) {
	c.identities.Invalidate(key)
	c.infos.Invalidate(key)
}
