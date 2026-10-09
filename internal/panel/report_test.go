package panel

import (
	"context"
	"net/http"
	"testing"
)

func TestReportRequiresBusinessAcknowledgement(t *testing.T) {
	for _, body := range []string{`{"data":false}`, `{}`, `{"data":"true"}`, `{"success":true}`, `not-json`, `{"data":true}`} {
		t.Run(body, func(t *testing.T) {
			server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			defer server.Close()
			err := client.Report(context.Background(), "batch-1", nil, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil)
			if (err == nil) != (body == `{"data":true}`) {
				t.Fatalf("Report error=%v", err)
			}
		})
	}
}

func TestReportHonorsCancelledContext(t *testing.T) {
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled request reached server") })
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Report(ctx, "batch", nil, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err == nil {
		t.Fatal("cancelled report succeeded")
	}
}
