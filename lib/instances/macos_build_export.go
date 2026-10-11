package instances

// MacOSBuildExportReceipt describes one stop/export operation, not persisted state.
// A failed export may still confirm that instance cleanup is safe.
type MacOSBuildExportReceipt struct{ Graceful, VMMExited bool }
