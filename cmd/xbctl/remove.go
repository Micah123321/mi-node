package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/micah123321/mi-node/internal/config"
)

func runRemove(args []string) error {
	nodeID, panel, err := parseRemoveArgs(args)
	if err != nil {
		return err
	}
	if err := ensureRoot("remove"); err != nil {
		return err
	}
	return removeNodeBinding(defaultConfigPath, defaultCredentialsPath, defaultMetaPath, nodeID, panel, runServiceCommand)
}

func removeNodeBinding(configPath, credentialsPath, metaPath string, nodeID int, panel string, serviceCommand func(string, ...string) error) error {
	root, err := loadWritableRootConfig(configPath)
	if err != nil {
		return err
	}
	instances := normalizeRootInstances(root)
	index, err := findRemoveNode(instances, nodeID, panel)
	if err != nil {
		return err
	}
	// Remove the selected entry from this snapshot, without matching its ID again.
	kept := make([]config.Config, 0, len(instances)-1)
	kept = append(kept, instances[:index]...)
	kept = append(kept, instances[index+1:]...)
	return persistRemovedBindings(root, kept, []config.Config{instances[index]}, configPath, credentialsPath, metaPath, serviceCommand)
}

func parseRemoveArgs(args []string) (int, string, error) {
	usage := errors.New("usage: xbctl remove <node-id> [--panel URL]")
	if len(args) != 1 && len(args) != 3 {
		return 0, "", usage
	}
	// Parse the entire ID: a typo such as 458abc must never delete node 458.
	nodeID, err := strconv.Atoi(args[0])
	if err != nil || nodeID <= 0 {
		return 0, "", fmt.Errorf("invalid node-id: %s", args[0])
	}
	var panel string
	if len(args) == 3 {
		if args[1] != "--panel" || strings.TrimSpace(args[2]) == "" {
			return 0, "", usage
		}
		panel = strings.TrimSpace(args[2])
	}
	return nodeID, panel, nil
}

func findRemoveNode(instances []config.Config, nodeID int, panel string) (int, error) {
	target := -1
	for i, inst := range instances {
		if inst.IsMachineMode() || inst.IsStandalone() || len(inst.Nodes) > 0 || inst.Panel.NodeID != nodeID {
			continue
		}
		if panel != "" && strings.TrimSpace(inst.Panel.URL) != panel {
			continue
		}
		if target >= 0 {
			return -1, fmt.Errorf("multiple bindings found for node_id=%d; use 'xbctl remove %d --panel URL' or 'xbctl bind remove <instance-id>'", nodeID, nodeID)
		}
		target = i
	}
	if target < 0 {
		return -1, fmt.Errorf("node binding not found: node_id=%d", nodeID)
	}
	return target, nil
}
