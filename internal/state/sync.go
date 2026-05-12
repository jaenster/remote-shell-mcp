package state

import (
	"fmt"
	"log/slog"

	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func Capture(ssh *sshx.Manager, dk *dockerx.Manager) *Snapshot {
	snap := &Snapshot{Version: 1}

	for _, s := range ssh.Sessions() {
		if !s.Spec.Persistent {
			continue
		}
		snap.SSHSessions = append(snap.SSHSessions, SSHSessionRecord{
			ID:   s.ID,
			Spec: ScrubSSH(s.Spec),
		})
		forwards, _ := ssh.ListForwards(s.ID)
		for _, f := range forwards {
			// Persist the user's original spec — not the effective port.
			// Otherwise BindPort=0 ("any free port") becomes pinned to the
			// ephemeral port we happened to pick this run, which can fail
			// to rebind on restart if the port is taken.
			rec := ForwardRecord{ID: f.ID, SessionID: s.ID, Kind: sshx.ForwardKind(f.Kind),
				LocalSpec:   f.LocalSpec,
				RemoteSpec:  f.RemoteSpec,
				DynamicSpec: f.DynamicSpec,
			}
			snap.Forwards = append(snap.Forwards, rec)
		}
	}

	for _, h := range dk.Hosts() {
		if !h.Spec.Persistent {
			continue
		}
		snap.DockerHosts = append(snap.DockerHosts, DockerHostRecord{
			ID:   h.ID,
			Spec: ScrubDocker(h.Spec),
		})
	}
	return snap
}

func Restore(snap *Snapshot, ssh *sshx.Manager, dk *dockerx.Manager, log *slog.Logger) {
	var sshOK, sshFail, fwdOK, fwdFail, dkOK, dkFail int
	for _, rec := range snap.SSHSessions {
		if _, err := ssh.Connect(rec.ID, rec.Spec); err != nil {
			sshFail++
			log.Warn("restore ssh session failed", "id", rec.ID, "err", err)
		} else {
			sshOK++
		}
	}
	for _, rec := range snap.Forwards {
		if err := restoreForward(rec, ssh); err != nil {
			fwdFail++
			log.Warn("restore forward failed", "id", rec.ID, "err", err)
		} else {
			fwdOK++
		}
	}
	for _, rec := range snap.DockerHosts {
		if _, err := dk.Connect(rec.ID, rec.Spec); err != nil {
			dkFail++
			log.Warn("restore docker host failed", "id", rec.ID, "err", err)
		} else {
			dkOK++
		}
	}
	// One summary line instead of one INFO per restored resource. Failures still
	// log individually above so the operator can see *which* one didn't come
	// back; successes are summarized to keep restart logs readable when there
	// are hundreds of persistent resources.
	log.Info("state restored",
		"ssh_sessions_ok", sshOK, "ssh_sessions_failed", sshFail,
		"forwards_ok", fwdOK, "forwards_failed", fwdFail,
		"docker_hosts_ok", dkOK, "docker_hosts_failed", dkFail,
	)
}

func restoreForward(rec ForwardRecord, ssh *sshx.Manager) error {
	switch rec.Kind {
	case sshx.ForwardLocal:
		if rec.LocalSpec == nil {
			return fmt.Errorf("missing local spec")
		}
		_, err := ssh.OpenLocalForward(rec.SessionID, rec.ID, *rec.LocalSpec)
		return err
	case sshx.ForwardRemote:
		if rec.RemoteSpec == nil {
			return fmt.Errorf("missing remote spec")
		}
		_, err := ssh.OpenRemoteForward(rec.SessionID, rec.ID, *rec.RemoteSpec)
		return err
	case sshx.ForwardDynamic:
		if rec.DynamicSpec == nil {
			return fmt.Errorf("missing dynamic spec")
		}
		_, err := ssh.OpenDynamicForward(rec.SessionID, rec.ID, *rec.DynamicSpec)
		return err
	}
	return fmt.Errorf("unknown kind %q", rec.Kind)
}
