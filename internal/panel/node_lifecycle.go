package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// NodeRemovedError is emitted only after the type-independent V2 handshake
// confirms twice that the authenticated node ID no longer exists.
type NodeRemovedError struct{ NodeID int }

func (e *NodeRemovedError) Error() string {
	return fmt.Sprintf("panel node %d no longer exists", e.NodeID)
}
func IsNodeRemoved(err error) bool {
	var removed *NodeRemovedError
	return errors.As(err, &removed)
}

// CheckNode uses the existing V2 handshake so a changed legacy node_type, a
// disabled listener, or a missing HTTP route cannot remove a valid binding.
func (c *Client) CheckNode(ctx context.Context) error {
	if c.machineID > 0 || c.nodeID <= 0 {
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		body, err := json.Marshal(map[string]any{"token": c.token, "node_id": c.nodeID})
		if err != nil {
			return err
		}
		resp, err := c.doRequestContext(ctx, http.MethodPost, "/api/v2/server/handshake", body, "")
		if err != nil {
			return fmt.Errorf("check panel node: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		drainAndClose(resp.Body)
		if readErr != nil || len(data) > 4096 {
			return fmt.Errorf("invalid node existence response")
		}
		if resp.StatusCode == http.StatusOK {
			var hs struct {
				WebSocket *json.RawMessage `json:"websocket"`
			}
			if json.Unmarshal(data, &hs) != nil || hs.WebSocket == nil {
				return fmt.Errorf("invalid node handshake response")
			}
			return nil
		}
		var failure struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if resp.StatusCode != http.StatusBadRequest || json.Unmarshal(data, &failure) != nil ||
			failure.Status != "fail" || failure.Message != "Server does not exist" {
			return fmt.Errorf("node existence check returned status %d", resp.StatusCode)
		}
	}
	return &NodeRemovedError{NodeID: c.nodeID}
}
