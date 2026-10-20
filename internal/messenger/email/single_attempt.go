package email

import "time"

// NewSingleAttempt creates a messenger whose delivery attempts are owned by a
// durable queue. smtppool starts its idle sweeper only when MaxConns > 1; using a
// single-connection pool retains server-closed idle sockets indefinitely.
func NewSingleAttempt(name string, servers ...Server) (*Emailer, error) {
	configured := append([]Server(nil), servers...)
	for i := range configured {
		configured[i].MaxMessageRetries = 1
		configured[i].MaxConns = 2
		if configured[i].IdleTimeout < time.Second {
			configured[i].IdleTimeout = 15 * time.Second
		}
	}
	return New(name, configured...)
}
