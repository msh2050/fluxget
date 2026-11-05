package testutil

import (
	"path/filepath"
	"testing"

	"github.com/msh2050/fluxget/internal/store"
	"github.com/msh2050/fluxget/internal/types"
	"github.com/msh2050/fluxget/internal/utils"
)

// SuppressNotificationsInTests disables desktop notifications for any test
// that imports this package. Call this from a per-package init() or TestMain.
func SuppressNotificationsInTests() {
	utils.SuppressNotifications = true
}

func init() {
	SuppressNotificationsInTests()
}

// SetupStateDB configures a fresh temp backend for tests that exercise state persistence.
func SetupStateDB(t *testing.T) string {
	t.Helper()

	tempDir := t.TempDir()
	store.CloseDB()
	store.Configure(filepath.Join(tempDir, "surge.db"))

	t.Cleanup(store.CloseDB)
	return tempDir
}

// SeedMasterList inserts a DownloadRecord into the master list for test setups.
func SeedMasterList(t *testing.T, entry types.DownloadRecord) {
	t.Helper()
	if err := store.AddToMasterList(entry); err != nil {
		t.Fatalf("SeedMasterList failed: %v", err)
	}
}
