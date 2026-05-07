package statute

// Shutdown configures graceful shutdown behaviour.
type Shutdown struct {
	// GracePeriod is the maximum time the server will wait for in-flight
	// requests to finish before forcibly closing connections. e.g. "30s".
	GracePeriod string

	// DrainListeners closes listeners (stops accepting new connections)
	// before waiting for in-flight requests. Recommended for production.
	DrainListeners bool
}
