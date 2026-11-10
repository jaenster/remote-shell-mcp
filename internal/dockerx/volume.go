package dockerx

import (
	"context"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// InspectVolume returns the volume object for volumeID, which may be a volume
// name or an id. The payload is the engine's object verbatim — driver,
// mountpoint on the host, scope, labels and driver options — so nothing is
// hidden from the caller. Mirrors Inspect for containers, which also hands
// back the engine's object unchanged.
func (h *Host) InspectVolume(ctx context.Context, volumeID string) (volume.Volume, error) {
	c, err := h.client()
	if err != nil {
		return volume.Volume{}, err
	}
	res, err := c.VolumeInspect(ctx, volumeID, client.VolumeInspectOptions{})
	if err != nil {
		return volume.Volume{}, err
	}
	return res.Volume, nil
}
