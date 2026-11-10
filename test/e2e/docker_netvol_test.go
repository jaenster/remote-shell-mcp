//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The Docker engine serialises network objects with capitalised keys
// ("Name", "Driver", "IPAM"), and the inspect tools hand the engine's object
// back verbatim, so the test decodes the engine's own shape.
type networkInspectResp struct {
	Name       string `json:"Name"`
	ID         string `json:"Id"`
	Driver     string `json:"Driver"`
	Scope      string `json:"Scope"`
	Created    string `json:"Created"`
	Internal   bool   `json:"Internal"`
	Attachable bool   `json:"Attachable"`
	IPAM       struct {
		Driver string `json:"Driver"`
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
	Labels map[string]string `json:"Labels"`
}

type volumeInspectResp struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Scope      string            `json:"Scope"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
}

// e2eLabelKey is the key half of containerLabel ("remote-shell-mcp-e2e"),
// which the Docker CLI stores as a label key rather than a "key=value" pair.
var e2eLabelKey = strings.SplitN(containerLabel, "=", 2)[0]

// TestDockerNetworkAndVolumeInspect exercises docker_network_inspect and
// docker_volume_inspect end-to-end.
//
// The resources are created with the docker CLI rather than through MCP:
// inspect is deliberately useful on its own, against networks and volumes that
// already exist — the default "bridge" network, a Compose-created network, a
// volume declared by someone else's `docker run -v`. The CLI is also the
// independent ground truth the MCP payload is checked against.
func TestDockerNetworkAndVolumeInspect(t *testing.T) {
	mc, _, _, teardown := setupSession(t)
	defer teardown()
	dockerHost := detectDockerHost()
	if dockerHost == "" {
		t.Skip("no docker socket detected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "docker_connect", map[string]any{
		"id": "d1", "spec": map[string]any{"host": dockerHost},
	}); err != nil {
		t.Fatalf("docker_connect: %v", err)
	}

	// --- network -------------------------------------------------------
	// Unique name: docker names are global per daemon, and CI jobs share one.
	netName := fmt.Sprintf("rsmcp-e2e-net-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "network", "create",
		"--label", containerLabel, netName).CombinedOutput()
	if err != nil {
		t.Fatalf("docker network create: %v\n%s", err, out)
	}
	netID := strings.TrimSpace(string(out))
	if netID == "" {
		t.Fatalf("docker network create returned no id")
	}
	// Remove the network through the CLI; this test only covers inspect.
	defer exec.Command("docker", "network", "rm", netName).Run()

	var net networkInspectResp
	if err := mc.CallJSON(ctx, "docker_network_inspect", map[string]any{
		"host_id": "d1", "network": netName,
	}, &net); err != nil {
		t.Fatalf("docker_network_inspect: %v", err)
	}
	if net.Name != netName {
		t.Fatalf("expected network name %q, got %q", netName, net.Name)
	}
	if net.ID == "" {
		t.Fatalf("expected a network id, got empty")
	}
	if net.Driver != "bridge" {
		t.Fatalf("expected driver bridge, got %q", net.Driver)
	}
	if net.Scope != "local" {
		t.Fatalf("expected scope local, got %q", net.Scope)
	}
	if net.Created == "" {
		t.Fatalf("expected a created timestamp, got empty")
	}
	if len(net.IPAM.Config) == 0 {
		t.Fatalf("expected IPAM config with at least one subnet, got %+v", net.IPAM)
	}
	if net.IPAM.Config[0].Subnet == "" {
		t.Fatalf("expected an IPAM subnet, got %+v", net.IPAM.Config[0])
	}
	if net.Labels[e2eLabelKey] != "1" {
		t.Fatalf("expected label %s=1, got %+v", e2eLabelKey, net.Labels)
	}

	// Inspecting by id must resolve to the same network.
	var netByID networkInspectResp
	if err := mc.CallJSON(ctx, "docker_network_inspect", map[string]any{
		"host_id": "d1", "network": netID,
	}, &netByID); err != nil {
		t.Fatalf("docker_network_inspect by id: %v", err)
	}
	if netByID.Name != netName {
		t.Fatalf("expected %q when inspecting by id, got %q", netName, netByID.Name)
	}

	// The default bridge network always exists — inspect must work on it
	// without the test creating anything.
	var bridge networkInspectResp
	if err := mc.CallJSON(ctx, "docker_network_inspect", map[string]any{
		"host_id": "d1", "network": "bridge",
	}, &bridge); err != nil {
		t.Fatalf("docker_network_inspect bridge: %v", err)
	}
	if bridge.Name != "bridge" {
		t.Fatalf("expected bridge, got %q", bridge.Name)
	}
	if bridge.Driver == "" {
		t.Fatalf("expected a driver for the bridge network")
	}

	// A missing network must surface as an error, not an empty object.
	if _, err := mc.CallText(ctx, "docker_network_inspect", map[string]any{
		"host_id": "d1", "network": "rsmcp-e2e-does-not-exist",
	}); err == nil {
		t.Fatalf("expected an error inspecting a nonexistent network")
	}

	// --- volume --------------------------------------------------------
	volName := fmt.Sprintf("rsmcp-e2e-vol-%d", time.Now().UnixNano())
	out, err = exec.Command("docker", "volume", "create",
		"--label", containerLabel, volName).CombinedOutput()
	if err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), volName) {
		t.Fatalf("docker volume create did not report %q: %s", volName, out)
	}
	defer exec.Command("docker", "volume", "rm", volName).Run()

	var vol volumeInspectResp
	if err := mc.CallJSON(ctx, "docker_volume_inspect", map[string]any{
		"host_id": "d1", "volume": volName,
	}, &vol); err != nil {
		t.Fatalf("docker_volume_inspect: %v", err)
	}
	if vol.Name != volName {
		t.Fatalf("expected volume name %q, got %q", volName, vol.Name)
	}
	if vol.Driver != "local" {
		t.Fatalf("expected driver local, got %q", vol.Driver)
	}
	if vol.Mountpoint == "" {
		t.Fatalf("expected a mountpoint, got empty")
	}
	if vol.Scope != "local" {
		t.Fatalf("expected scope local, got %q", vol.Scope)
	}
	if vol.CreatedAt == "" {
		t.Fatalf("expected a created-at timestamp, got empty")
	}
	if vol.Labels[e2eLabelKey] != "1" {
		t.Fatalf("expected label %s=1, got %+v", e2eLabelKey, vol.Labels)
	}

	// The mountpoint must match what the CLI reports — the whole point of
	// the tool is that the model gets the real path on the host.
	cliOut, err := exec.Command("docker", "volume", "inspect",
		"--format", "{{.Mountpoint}}", volName).CombinedOutput()
	if err != nil {
		t.Fatalf("docker volume inspect: %v\n%s", err, cliOut)
	}
	if want := strings.TrimSpace(string(cliOut)); vol.Mountpoint != want {
		t.Fatalf("mountpoint mismatch: mcp=%q cli=%q", vol.Mountpoint, want)
	}

	// A missing volume must surface as an error, not an empty object.
	if _, err := mc.CallText(ctx, "docker_volume_inspect", map[string]any{
		"host_id": "d1", "volume": "rsmcp-e2e-no-such-volume",
	}); err == nil {
		t.Fatalf("expected an error inspecting a nonexistent volume")
	}
}
