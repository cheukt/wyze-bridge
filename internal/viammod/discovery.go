package viammod

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
)

const discoveryRetryInterval = 10 * time.Second

// awaitInitialDiscovery retries discovery every interval until an attempt
// succeeds, reporting false if ctx is cancelled first. RunDiscoveryLoop retries
// only on its 30-minute refresh tick, so a transient Wyze failure at startup
// (usually a 429) would otherwise cost half an hour of no cameras.
//
// Callers run RunDiscoveryLoop after this returns, never alongside it, so
// discovery keeps a single caller. The cost is one duplicate get_object_list,
// since RunDiscoveryLoop opens with a discovery of its own.
func awaitInitialDiscovery(ctx context.Context, camMgr *camera.Manager, interval time.Duration, log zerolog.Logger) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		err := camMgr.Discover(ctx)
		if err == nil {
			return true
		}
		log.Warn().Err(err).Dur("retry_in", interval).Msg("initial discovery failed, retrying")

		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}
