package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var bindingWriteMu sync.Mutex

// RemoveNodeBinding removes only the static binding for this panel and node.
func RemoveNodeBinding(path, panelURL string, nodeID int) (bool, error) {
	return removeNodeBinding(path, panelURL, nodeID, nil)
}

// RemoveNodeBindingIfUnchanged ignores stale deletion results after reconfiguration.
func RemoveNodeBindingIfUnchanged(path string, expected *Config) (bool, error) {
	if expected == nil || expected.IsMachineMode() || expected.IsStandalone() {
		return false, nil
	}
	return removeNodeBinding(path, expected.Panel.URL, expected.Panel.NodeID, expected)
}

func removeNodeBinding(path, panelURL string, nodeID int, expected *Config) (bool, error) {
	bindingWriteMu.Lock()
	defer bindingWriteMu.Unlock()
	if nodeID <= 0 || strings.TrimSpace(panelURL) == "" {
		return false, fmt.Errorf("invalid node binding")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	original, err := os.ReadFile(canonical)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("config root must be a mapping")
	}
	runtimeRoot, err := LoadRoot(canonical)
	if err != nil {
		return false, err
	}
	runtimeInstances, err := runtimeRoot.NormalizeInstances()
	if err != nil {
		return false, err
	}
	if expected != nil {
		matched := false
		for _, inst := range runtimeInstances {
			if inst.IsMachineMode() || inst.IsStandalone() {
				continue
			}
			for _, node := range inst.ExpandNodes() {
				if samePanelURL(node.Panel.URL, panelURL) && node.Panel.NodeID == nodeID &&
					node.InstanceID == expected.InstanceID && node.Panel.Token == expected.Panel.Token {
					matched = true
				}
			}
		}
		if !matched {
			return false, nil
		}
	}
	root := doc.Content[0]
	instances := mappingValue(root, "instances")
	if instances != nil {
		if instances.Kind != yaml.SequenceNode {
			return false, fmt.Errorf("instances must be a sequence")
		}
		if len(instances.Content) != len(runtimeInstances) {
			return false, fmt.Errorf("instance layout changed")
		}
		kept := make([]*yaml.Node, 0, len(instances.Content))
		removed := false
		for i, item := range instances.Content {
			inst := runtimeInstances[i]
			sameOwner := expected == nil || inst.InstanceID == expected.InstanceID
			changed, empty := false, false
			if sameOwner {
				changed, empty = removeBindingEntry(item, inst, panelURL, nodeID)
			}
			removed = removed || changed
			if empty {
				continue
			}
			// Pin directories before a shorter instance/node list changes derived defaults.
			pinRuntimePaths(item, inst)
			kept = append(kept, item)
		}
		if !removed {
			return false, nil
		}
		instances.Content = kept
		if len(kept) == 0 {
			removeMappingKey(root, "panel")
		}
	} else {
		if len(runtimeInstances) != 1 {
			return false, nil
		}
		removed, empty := removeBindingEntry(root, runtimeInstances[0], panelURL, nodeID)
		if !removed {
			return false, nil
		}
		if empty {
			removeMappingKey(root, "panel")
			removeMappingKey(root, "nodes")
			setMappingValue(root, "instances", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		} else {
			pinRuntimePaths(root, runtimeInstances[0])
		}
	}
	if err := writeYAMLAtomically(canonical, original, &doc); err != nil {
		return false, err
	}
	return true, nil
}

func removeBindingEntry(item *yaml.Node, inst *Config, panelURL string, nodeID int) (bool, bool) {
	if inst.IsMachineMode() || inst.IsStandalone() || !samePanelURL(inst.Panel.URL, panelURL) {
		return false, false
	}
	if len(inst.Nodes) == 0 {
		return inst.Panel.NodeID == nodeID, inst.Panel.NodeID == nodeID
	}
	nodes := mappingValue(item, "nodes")
	if nodes == nil || nodes.Kind != yaml.SequenceNode {
		return false, false
	}
	for i, node := range nodes.Content {
		if scalarInt(mappingValue(node, "node_id")) == nodeID {
			nodes.Content = append(nodes.Content[:i], nodes.Content[i+1:]...)
			return true, len(nodes.Content) == 0
		}
	}
	return false, false
}

func pinRuntimePaths(item *yaml.Node, inst *Config) {
	kernel := mappingValue(item, "kernel")
	if kernel == nil {
		kernel = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(item, "kernel", kernel)
	}
	if kernel.Kind == yaml.MappingNode {
		setMappingValue(kernel, "config_dir", stringNode(inst.Kernel.ConfigDir))
		setMappingValue(kernel, "geo_data_dir", stringNode(inst.Kernel.GeoDataDir))
	}
	cert := mappingValue(item, "cert")
	if cert == nil {
		cert = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(item, "cert", cert)
	}
	if cert.Kind == yaml.MappingNode {
		setMappingValue(cert, "cert_dir", stringNode(inst.Cert.CertDir))
	}
}

func samePanelURL(a, b string) bool {
	na, _, ea := normalizeBaseURL(a)
	nb, _, eb := normalizeBaseURL(b)
	return ea == nil && eb == nil && na == nb
}
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, stringNode(key), value)
}
func stringNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}
func removeMappingKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}
func scalarInt(node *yaml.Node) int {
	if node == nil || node.Kind != yaml.ScalarNode {
		return 0
	}
	value, _ := strconv.Atoi(node.Value)
	return value
}
func writeYAMLAtomically(path string, original []byte, doc *yaml.Node) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".mi-node-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(original, current) {
		return fmt.Errorf("config changed during node removal; retry required")
	}
	return os.Rename(temp.Name(), path)
}
