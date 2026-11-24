package db

import (
	"context"
	"database/sql"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeAuthBackend is a PostgreSQL server stand-in that answers the startup message with one chosen
// authentication message and records whether the client ever sent a password.
type fakeAuthBackend struct {
	addr string

	mu           sync.Mutex
	sentPassword bool
}

func (b *fakeAuthBackend) gotPassword() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sentPassword
}

// startFakeAuthBackend listens on a loopback port. reply is the authentication request sent after the startup
// message. When the client goes on (answers with a password), the fake accepts it and finishes the handshake,
// so a client that does not enforce its authentication rules ends up connected.
func startFakeAuthBackend(t *testing.T, reply pgproto3.BackendMessage) *fakeAuthBackend {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeAuthBackend{addr: l.Addr().String()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go b.serve(c, reply)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
	})
	return b
}

func (b *fakeAuthBackend) serve(c net.Conn, reply pgproto3.BackendMessage) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	be := pgproto3.NewBackend(c, c)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		return
	}
	be.Send(reply)
	if err := be.Flush(); err != nil {
		return
	}
	if _, ok := reply.(*pgproto3.AuthenticationOk); !ok {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.PasswordMessage); ok {
			b.mu.Lock()
			b.sentPassword = true
			b.mu.Unlock()
			be.Send(&pgproto3.AuthenticationOk{})
		} else {
			return
		}
	}
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	_ = be.Flush()
	// Keep the connection until the client closes it.
	for {
		if _, err := be.Receive(); err != nil {
			return
		}
	}
}

func connectFake(b *fakeAuthBackend, extra string) error {
	dsn := "postgres://owner:secret-for-the-fake@" + b.addr + "/app?sslmode=disable&connect_timeout=5" + extra
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Conn opens one driver connection (the handshake) and sends no query, which the fake would not answer.
	c, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	return c.Close()
}

// The owner connection of a deployment that runs the Signal sidecar sets require_auth=scram-sha-256: a server
// (or a process that took over its socket) that asks for a clear-text or MD5 password, or that says the client
// is authenticated without asking for anything, must be refused, and the password must never be sent.
func TestRequireAuthScramRefusesWeakAuthentication(t *testing.T) {
	cases := []struct {
		name  string
		reply pgproto3.BackendMessage
	}{
		{"clear-text password request", &pgproto3.AuthenticationCleartextPassword{}},
		{"md5 password request", &pgproto3.AuthenticationMD5Password{Salt: [4]byte{1, 2, 3, 4}}},
		{"ok without any request", &pgproto3.AuthenticationOk{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := startFakeAuthBackend(t, tc.reply)
			if err := connectFake(b, "&require_auth=scram-sha-256"); err == nil {
				t.Fatal("connect succeeded although only scram-sha-256 is allowed")
			}
			if b.gotPassword() {
				t.Fatal("the client sent a password to a server that did not offer scram-sha-256")
			}
		})
	}
}

// Control: without the rule the same fake backend is accepted, so the refusals above come from require_auth
// and not from a defect of the fake.
func TestRequireAuthControlWithoutRuleConnects(t *testing.T) {
	b := startFakeAuthBackend(t, &pgproto3.AuthenticationCleartextPassword{})
	if err := connectFake(b, ""); err != nil {
		t.Fatalf("control connect failed: %v", err)
	}
	if !b.gotPassword() {
		t.Fatal("control: the fake did not see the clear-text password")
	}
}
