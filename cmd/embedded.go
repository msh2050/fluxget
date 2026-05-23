package cmd

// StartEmbedded starts the FluxGet HTTP server for embedded use (Wails GUI).
// It initialises the download engine, binds to port, and begins serving.
// Call ShutdownEmbedded when the host application exits.
func StartEmbedded(port int, outputDir string) error {
	// Initialise state DB, logging, and directory structure — same as CLI startup.
	if err := initializeGlobalState(); err != nil {
		return err
	}

	_, listener, err := bindServerListener(port)
	if err != nil {
		return err
	}
	resetGlobalEnqueueContext()
	if err := ensureGlobalLocalServiceAndLifecycle(); err != nil {
		return err
	}
	saveActivePort(port)
	go startHTTPServer(listener, port, outputDir, GlobalService, "")
	StartHeadlessConsumer()
	resumePausedDownloads()
	return nil
}

// ShutdownEmbedded performs a graceful shutdown of the embedded server.
func ShutdownEmbedded() error {
	removeActivePort()
	return executeGlobalShutdown("embedded: shutdown requested")
}
