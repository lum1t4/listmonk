package email

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/knadh/smtppool/v2"
)

// A real SMTP peer that drops idle connections, as providers do in production.
func smtpPeer(t *testing.T, reject bool) (string, int, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections, deliveries, commands atomic.Int32
	var handlers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				respond := func(s string) bool { _, err := fmt.Fprintf(conn, "%s\r\n", s); return err == nil }
				if !respond("220 test SMTP") {
					return
				}
				for {
					conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						if !respond("250 test") {
							return
						}
					case strings.HasPrefix(line, "MAIL"):
						commands.Add(1)
						if reject {
							if !respond("451 Temporary failure") {
								return
							}
						} else if !respond("250 Sender accepted") {
							return
						}
					case strings.HasPrefix(line, "RCPT"), strings.HasPrefix(line, "RSET"), strings.HasPrefix(line, "NOOP"):
						if !respond("250 OK") {
							return
						}
					case strings.HasPrefix(line, "DATA"):
						if !respond("354 End with dot") {
							return
						}
						for {
							line, err = reader.ReadString('\n')
							if err != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
						}
						deliveries.Add(1)
						if !respond("250 Accepted") {
							return
						}
					case strings.HasPrefix(line, "QUIT"):
						respond("221 Bye")
						return
					default:
						respond("500 Unknown command")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done; handlers.Wait() })
	address := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", address.Port, &connections, &deliveries, &commands
}

func TestSingleAttemptClosesIdleConnections(t *testing.T) {
	host, port, connections, deliveries, _ := smtpPeer(t, false)
	server := Server{TLSType: "none", Opt: smtppool.Opt{Host: host, Port: port, MaxConns: 1, MaxMessageRetries: 5, IdleTimeout: time.Second, PoolWaitTimeout: time.Second}}
	messenger, err := NewSingleAttempt("test", server)
	if err != nil {
		t.Fatal(err)
	}
	defer messenger.Close()
	msg := models.Message{From: "hello@example.test", To: []string{"subscriber@example.test"}, Subject: "Welcome", Body: []byte("Welcome"), ContentType: "plain"}
	if err := messenger.Push(msg); err != nil {
		t.Fatal(err)
	}
	// The upstream sweeper ticks at 2 seconds and the peer closes at 2.5 seconds.
	// At 3.2 seconds an unswept, single-connection pool reuses a dead socket.
	time.Sleep(3200 * time.Millisecond)
	if err := messenger.Push(msg); err != nil {
		t.Fatalf("second welcome after idle failed: %v", err)
	}
	if deliveries.Load() != 2 || connections.Load() != 2 {
		t.Fatalf("expected two deliveries on fresh connections: messages=%d connections=%d", deliveries.Load(), connections.Load())
	}
	if server.MaxConns != 1 || server.MaxMessageRetries != 5 {
		t.Fatal("caller configuration was mutated")
	}
}

func TestSingleAttemptDoesNotRetryInsideSMTPPool(t *testing.T) {
	host, port, _, deliveries, commands := smtpPeer(t, true)
	messenger, err := NewSingleAttempt("test", Server{TLSType: "none", Opt: smtppool.Opt{Host: host, Port: port, MaxConns: 10, MaxMessageRetries: 5, IdleTimeout: time.Second, PoolWaitTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer messenger.Close()
	err = messenger.Push(models.Message{From: "hello@example.test", To: []string{"subscriber@example.test"}, Subject: "Welcome", Body: []byte("Welcome"), ContentType: "plain"})
	if err == nil {
		t.Fatal("expected temporary SMTP rejection")
	}
	// A single MAIL command must be attempted, regardless of the configured retry count.
	if commands.Load() != 1 {
		t.Fatalf("SMTP pool performed %d attempts; queue must own retries", commands.Load())
	}
	if deliveries.Load() != 0 {
		t.Fatal("rejected message was accepted")
	}
}
