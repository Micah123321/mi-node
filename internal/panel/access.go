package panel

import (
	"context"
	"errors"
	"github.com/micah123321/mi-node/internal/access"
)

// ErrAccessReportExpired marks a permanently expired detail batch (HTTP 410).
var ErrAccessReportExpired = errors.New("access report retention expired")

// AccessReport is independently acknowledged and never contributes to charging.
type AccessReport struct {
	Version         int              `json:"version"`
	ReportID        string           `json:"report_id"`
	BillingReportID string           `json:"billing_report_id"`
	Kernel          string           `json:"kernel"`
	ObservedAt      int64            `json:"observed_at"`
	Coverage        string           `json:"coverage"`
	DroppedEntries  int64            `json:"dropped_entries"`
	Totals          map[int][2]int64 `json:"totals"`
	Entries         []access.Entry   `json:"entries"`
}

func (c *Client) ReportAccess(ctx context.Context, report AccessReport) error {
	return c.postJSONContext(ctx, "/api/v2/server/access/report", map[string]interface{}{
		"version": report.Version, "report_id": report.ReportID,
		"billing_report_id": report.BillingReportID, "kernel": report.Kernel,
		"observed_at": report.ObservedAt, "coverage": report.Coverage,
		"dropped_entries": report.DroppedEntries, "totals": report.Totals, "entries": report.Entries,
	})
}
