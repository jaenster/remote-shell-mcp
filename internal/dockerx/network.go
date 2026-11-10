package dockerx

import (
	"context"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// InspectNetwork returns the network object for networkID, which may be a
// network name or an id. The payload is the engine's object verbatim —
// driver, scope, IPAM configuration (subnets, ranges, gateways), labels,
// driver options and the containers currently attached to it — so nothing is
// hidden from the caller. Mirrors Inspect for containers, which also hands
// back the engine's object unchanged.
func (h *Host) InspectNetwork(ctx context.Context, networkID string) (network.Inspect, error) {
	c, err := h.client()
	if err != nil {
		return network.Inspect{}, err
	}
	res, err := c.NetworkInspect(ctx, networkID, client.NetworkInspectOptions{})
	if err != nil {
		return network.Inspect{}, err
	}
	return res.Network, nil
}
