package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/nlog"
)

var nodeRemovalMu sync.Mutex

func removeDeletedNodeBinding(configPath string, node *config.Config) error {
	nodeRemovalMu.Lock()
	defer nodeRemovalMu.Unlock()
	removed, err := config.RemoveNodeBindingIfUnchanged(configPath, node)
	if err != nil || !removed {
		return err
	}
	root, err := config.LoadRoot(configPath)
	if err != nil {
		return err
	}
	remaining, err := root.NormalizeInstances()
	if err != nil {
		return err
	}
	if !canRemoveNodeDir(configPath, node, remaining) {
		return nil
	}
	if err := os.RemoveAll(node.Kernel.ConfigDir); err != nil {
		nlog.Core().Warn("node binding removed; runtime directory cleanup deferred", "node_id", node.Panel.NodeID, "error", err)
	}
	return nil
}

// Only generated, isolated directories are disposable. Custom directories and
// paths referenced by a remaining instance stay under operator control.
func canRemoveNodeDir(configPath string, node *config.Config, remaining []*config.Config) bool {
	dir, err := filepath.Abs(node.Kernel.ConfigDir)
	if err != nil || strings.TrimSpace(node.Kernel.ConfigDir) == "" {
		return false
	}
	base, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	generated := rel == node.InstanceID || rel == filepath.Join("instances", node.InstanceID) ||
		strings.HasSuffix(rel, string(filepath.Separator)+fmt.Sprintf("node-%d", node.Panel.NodeID)) || rel == fmt.Sprintf("node-%d", node.Panel.NodeID)
	if !generated {
		return false
	}
	for part := dir; ; part = filepath.Dir(part) {
		info, err := os.Lstat(part)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	for _, inst := range remaining {
		configs := []*config.Config{inst}
		if !inst.IsMachineMode() {
			configs = append(configs, inst.ExpandNodes()...)
		}
		for _, cfg := range configs {
			for _, used := range []string{cfg.Kernel.ConfigDir, cfg.Kernel.GeoDataDir, cfg.Kernel.CustomConfig, cfg.Cert.CertDir, cfg.Cert.CertFile, cfg.Cert.KeyFile} {
				if used == "" {
					continue
				}
				if pathsOverlap(dir, used) {
					return false
				}
			}
		}
	}
	return !pathsOverlap(dir, configPath)
}

func pathsOverlap(a, b string) bool {
	aa, ea := filepath.Abs(a)
	bb, eb := filepath.Abs(b)
	if ea != nil || eb != nil {
		return true
	}
	if real, err := filepath.EvalSymlinks(aa); err == nil {
		aa = real
	}
	if real, err := filepath.EvalSymlinks(bb); err == nil {
		bb = real
	}
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(aa, bb) || within(bb, aa)
}

func waitForNodeBindings(ctx context.Context, path string) (*config.RootConfig, error) {
	updates := make(chan *config.RootConfig, 1)
	watcher, err := config.WatchConfigRoot(ctx, path, func(root *config.RootConfig) {
		instances, err := root.NormalizeInstances()
		if err != nil || len(instances) == 0 {
			return
		}
		select {
		case updates <- root:
		default:
		}
	})
	if err != nil {
		return nil, err
	}
	defer watcher.Stop()
	// Subscribe before reading, so a binding added during startup is not missed.
	current, err := config.LoadRoot(path)
	if err != nil {
		return nil, err
	}
	instances, err := current.NormalizeInstances()
	if err != nil {
		return nil, err
	}
	if len(instances) > 0 {
		return current, nil
	}
	select {
	case root := <-updates:
		return root, nil
	case <-ctx.Done():
		return nil, nil
	}
}
