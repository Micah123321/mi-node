package controlplane

import (
	"context"
	"github.com/micah123321/mi-node/internal/panel"
)

type AccessReport = panel.AccessReport

// AccessReporter is optional so standalone sources and existing sinks retain compatibility.
type AccessReporter interface {
	ReportAccess(context.Context, AccessReport) error
}

func (p *PanelControlPlane) ReportAccess(ctx context.Context, payload AccessReport) error {
	return p.client.ReportAccess(ctx, payload)
}

func (p *MachinePanelControlPlane) ReportAccess(ctx context.Context, payload AccessReport) error {
	return p.client.ReportAccess(ctx, payload)
}
