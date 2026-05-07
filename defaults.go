package statute

// Defaults sets the conservative production baseline for all listeners. Routes
// may override individual values via middleware.
type Defaults struct {
	// ReadHeaderTimeout caps how long a client may take to send the request
	// headers. The Go standard library has no default; setting this is the
	// primary mitigation for Slowloris denial-of-service.
	ReadHeaderTimeout string

	// ReadTimeout caps the entire request read, including body. Use with care
	// for streaming and long-poll endpoints.
	ReadTimeout string

	// WriteTimeout caps how long the server may take to write the response.
	WriteTimeout string

	// IdleTimeout caps how long an idle keep-alive connection may sit between
	// requests before being closed.
	IdleTimeout string

	// MaxHeaderBytes caps the size of the request header block. Defaults to
	// the Go standard library default of 1MB when unset.
	MaxHeaderBytes int
}
