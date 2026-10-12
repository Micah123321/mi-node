package panel

import (
	"sync"
	"sync/atomic"

	"github.com/micah123321/mi-node/internal/discovery"
)

var inventoryOnce sync.Once
var processInventory atomic.Pointer[discovery.Inventory]
var inventoryError error
var inventoryDirectory string

// InitUpdateInventory freezes process-wide discovery metadata before clients start.
// An empty directory uses the system default. Failure disables discovery only.
func InitUpdateInventory(version, directory string) error {
	inventoryOnce.Do(func() {
		inventory, err := discovery.Load(directory, version)
		inventoryError = err
		if err == nil {
			// Program capability is independent of enrollment and update policy.
			inventory.AutoEnrollSupported = true
			inventoryDirectory = directory
			processInventory.Store(&inventory)
		}
	})
	return inventoryError
}

// Only stable metadata is frozen; enrollment telemetry is read for every send.
func currentUpdateInventory() *discovery.Inventory {
	inventory := processInventory.Load()
	if inventory == nil {
		return nil
	}
	snapshot := *inventory
	snapshot.EnrollmentStatus = discovery.ReadEnrollmentStatus(inventoryDirectory)
	return &snapshot
}
