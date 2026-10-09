//go:build !linux

package updateagent

import "fmt"

func syncDir(string) error        { return nil }
func secureFile(string) error     { return fmt.Errorf("unsupported_os") }
func Lock(string) (func(), error) { return nil, fmt.Errorf("unsupported_os") }
