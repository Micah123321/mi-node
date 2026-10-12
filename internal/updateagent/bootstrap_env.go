package updateagent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/micah123321/mi-node/internal/config"
)

// Keep this allowlist aligned with Config.applyEnvOverrides. The service needs
// the same bindings as mi-node, not the rest of its process environment.
var bootstrapConfigEnvironment = []string{
	"apiHost", "API_HOST", "apiKey", "API_KEY", "nodeID", "NODE_ID",
	"nodeType", "NODE_TYPE", "kernel", "KERNEL_TYPE", "certFile", "CERT_FILE",
	"keyFile", "KEY_FILE", "domain", "DOMAIN", "logLevel", "LOG_LEVEL",
	"MACHINE_ID", "MACHINE_TOKEN",
}

func bootstrapEnvironment(configPath string) ([]byte, error) {
	root, err := config.LoadRoot(configPath)
	if err != nil {
		return nil, fmt.Errorf("load bootstrap environment bindings failed")
	}
	names := make(map[string]bool, len(bootstrapConfigEnvironment))
	for _, name := range bootstrapConfigEnvironment {
		names[name] = true
	}
	addRefs := func(c *config.Config) {
		if c.Panel.TokenEnv != "" {
			names[c.Panel.TokenEnv] = true
		}
		if c.Machine != nil && c.Machine.TokenEnv != "" {
			names[c.Machine.TokenEnv] = true
		}
	}
	if len(root.Instances) == 0 {
		addRefs(&root.Config)
	} else {
		for i := range root.Instances {
			addRefs(&root.Instances[i])
		}
	}
	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, name := range keys {
		value, exists := os.LookupEnv(name)
		if !exists {
			continue
		}
		if !validBootstrapEnvName(name) || !validBootstrapEnvValue(value) {
			return nil, fmt.Errorf("invalid bootstrap environment assignment")
		}
		// EnvironmentFile uses shell-style double quotes, not Go/C escapes.
		// Literal newlines are preserved; percent signs are not specifiers here.
		value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "\x60", "\\\x60").Replace(value)
		out.WriteString(name + "=\"" + value + "\"\n")
	}
	return []byte(out.String()), nil
}

func validBootstrapEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range []byte(name) {
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func validBootstrapEnvValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c == 0 || c == '\uFEFF' || c >= '\uFDD0' && c <= '\uFDEF' || c&0xFFFF >= 0xFFFE {
			return false
		}
	}
	return true
}

func writeBootstrapEnvironment(configPath, stateDirectory string) error {
	data, err := bootstrapEnvironment(configPath)
	if err != nil {
		return err
	}
	directory := filepath.Join(stateDirectory, "bootstrap")
	for _, path := range []string{stateDirectory, directory} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return fmt.Errorf("create bootstrap environment directory failed")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("bootstrap environment directory must not be a symlink")
		}
		if os.Geteuid() >= 0 {
			if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
				return fmt.Errorf("protect bootstrap environment directory ownership failed")
			}
		}
		if err := os.Chmod(path, 0700); err != nil {
			return fmt.Errorf("protect bootstrap environment directory failed")
		}
	}
	path := filepath.Join(directory, "environment.env")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bootstrap environment must be a regular file")
		}
		if os.Geteuid() >= 0 {
			if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
				return fmt.Errorf("protect bootstrap environment ownership failed")
			}
		}
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("protect bootstrap environment failed")
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read bootstrap environment failed")
		}
		if bytes.Equal(current, data) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect bootstrap environment failed")
	}
	if err := atomicBytes(path, data, 0600); err != nil {
		return fmt.Errorf("write bootstrap environment failed")
	}
	return nil
}
