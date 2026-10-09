package updateagent

import (
	"context"
	"fmt"
	"os"
)

// InstallManual requires the caller to hold Lock for e.Paths[0] throughout
// staging and installation. Local hashes support recovery, not release authenticity.
func (e *Engine) InstallManual(ctx context.Context, staged [2]string, version string) error {
	if err := e.ManualAllowed(); err != nil {
		return err
	}
	if err := os.MkdirAll(e.Dir, 0700); err != nil {
		return err
	}
	t := &Transaction{ClaimID: UUID(), Manual: true, TargetVersion: version, Phase: "preparing", OldVersions: observeVersions(e.Paths)}
	if err := e.Prepare(t, staged); err != nil {
		return err
	}
	if err := e.Install(ctx, t); err != nil {
		return err
	}
	if t.Phase != "succeeded" {
		return fmt.Errorf("manual upgrade %s; journal and backups retained", t.Phase)
	}
	return nil
}
