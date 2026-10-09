package panel

import (
	"sync"
	"sync/atomic"

	"github.com/micah123321/mi-node/internal/discovery"
)

var inventoryOnce sync.Once
var processInventory atomic.Pointer[discovery.Inventory]
var inventoryError error

// InitUpdateInventory freezes process-wide discovery metadata before clients start.
// An empty directory uses the system default. Failure disables discovery only.
func InitUpdateInventory(version, directory string) error {
	inventoryOnce.Do(func() {
		inventory, err := discovery.Load(directory, version)
		inventoryError = err
		if err == nil {
			processInventory.Store(&inventory)
		}
	})
	return inventoryError
}
