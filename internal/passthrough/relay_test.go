package passthrough

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/packet"
)

type testRelay struct {
	secure bool
	reply  []byte
}

func (r *testRelay) ClientOnTLS() bool                        { return r.secure }
func (r *testRelay) Challenge(string, []byte) ([]byte, error) { return r.reply, nil }
func (r *testRelay) Relay([]byte) ([]byte, error)             { return r.reply, nil }
func (*testRelay) Notify([]byte) error                        { return nil }
func (*testRelay) Log(string)                                 {}

func TestChangeUserRequiresClientTLS(t *testing.T) {
	_, err := DialChangeUser(context.Background(), "unused", "bootstrap", "secret", "user", &testRelay{}, time.Second, nil)
	if err == nil {
		t.Fatal("accepted plaintext client relay")
	}
}

func TestFullAuthenticationIsOpaque(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	left.SetDeadline(time.Now().Add(time.Second))
	right.SetDeadline(time.Now().Add(time.Second))
	reply := []byte{0, 255, 1, 0, 127} // Preserve all bytes, including embedded and trailing NULs.
	result := make(chan error, 1)
	go func() {
		pc := packet.NewConn(right)
		if err := pc.WritePacket([]byte{0, 0, 0, 0, mysql.MORE_DATE_HEADER, mysql.CACHE_SHA2_FULL_AUTH}); err != nil {
			result <- err
			return
		}
		got, err := pc.ReadPacket()
		if err != nil {
			result <- err
			return
		}
		if !bytes.Equal(got, reply) {
			result <- errors.New("relay modified authentication bytes")
			return
		}
		result <- pc.WritePacket([]byte{0, 0, 0, 0, mysql.OK_HEADER, 0, 0, 2, 0, 0, 0})
	}()
	if err := relayConnectionPhase(packet.NewConn(left), &testRelay{secure: true, reply: reply}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamAuthenticationErrorPreserved(t *testing.T) {
	err := parseErrPacket(append([]byte{mysql.ERR_HEADER, 0x15, 0x04}, []byte("#28000Access denied")...))
	if err.Code != 1045 || err.State != "28000" || err.Message != "Access denied" {
		t.Fatalf("lost upstream error: %v", err)
	}
}

// serveUpstream runs fn as a fake upstream on one end of a pipe and returns the
// relay-side packet connection. Both ends carry a deadline so a bug fails the
// test instead of hanging it.
func serveUpstream(t *testing.T, fn func(pc *packet.Conn)) *packet.Conn {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	left.SetDeadline(time.Now().Add(2 * time.Second))
	right.SetDeadline(time.Now().Add(2 * time.Second))
	go fn(packet.NewConn(right))
	return packet.NewConn(left)
}

func authSwitch(plugin string, salt []byte) []byte {
	p := append([]byte{0, 0, 0, 0, mysql.EOF_HEADER}, plugin...)
	p = append(p, 0)
	p = append(p, salt...)
	return append(p, 0)
}

func TestRelayAuthSwitchThenOK(t *testing.T) {
	var gotPlugin string
	var gotSalt []byte
	relay := &recordingRelay{testRelay: testRelay{secure: true, reply: []byte{9, 9}}, plugin: &gotPlugin, salt: &gotSalt}
	salt := bytes.Repeat([]byte{7}, 20)
	var replied []byte
	pc := serveUpstream(t, func(pc *packet.Conn) {
		pc.WritePacket(authSwitch("caching_sha2_password", salt))
		replied, _ = pc.ReadPacket()
		pc.WritePacket([]byte{0, 0, 0, 0, mysql.OK_HEADER, 0, 0, 2, 0, 0, 0})
	})
	if err := relayConnectionPhase(pc, relay); err != nil {
		t.Fatal(err)
	}
	if gotPlugin != "caching_sha2_password" || !bytes.Equal(gotSalt, salt) {
		t.Fatalf("challenge got plugin %q salt % x", gotPlugin, gotSalt)
	}
	if !bytes.Equal(replied, []byte{9, 9}) {
		t.Fatalf("upstream received % x", replied)
	}
}

type recordingRelay struct {
	testRelay
	plugin *string
	salt   *[]byte
}

func (r *recordingRelay) Challenge(plugin string, salt []byte) ([]byte, error) {
	*r.plugin = plugin
	*r.salt = append([]byte(nil), salt...)
	return r.reply, nil
}

func TestRelayStopsAfterTooManyRounds(t *testing.T) {
	pc := serveUpstream(t, func(pc *packet.Conn) {
		for i := 0; i < maxAuthRounds*3; i++ {
			if err := pc.WritePacket(authSwitch("mysql_native_password", make([]byte, 20))); err != nil {
				return
			}
			if _, err := pc.ReadPacket(); err != nil {
				return
			}
		}
	})
	err := relayConnectionPhase(pc, &testRelay{secure: true, reply: []byte{1}})
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected the round limit to stop the exchange, got %v", err)
	}
}

func TestRelayRejectsMalformedUpstreamPackets(t *testing.T) {
	for name, packetBody := range map[string][]byte{
		"empty packet":           {},
		"unknown header":         {0x42, 1, 2},
		"switch without NUL":     append([]byte{mysql.EOF_HEADER}, "no_terminator"...),
		"switch with empty name": {mysql.EOF_HEADER, 0, 1, 2},
		"switch with huge name":  append(append([]byte{mysql.EOF_HEADER}, bytes.Repeat([]byte{'a'}, maxPluginNameLen+1)...), 0),
		"switch with huge salt":  append(append([]byte{mysql.EOF_HEADER, 'p', 0}, bytes.Repeat([]byte{1}, maxAuthChallengeLen+1)...), 0),
		"huge more-data":         append([]byte{mysql.MORE_DATE_HEADER}, bytes.Repeat([]byte{1}, maxAuthChallengeLen+1)...),
	} {
		t.Run(name, func(t *testing.T) {
			pc := serveUpstream(t, func(pc *packet.Conn) {
				pc.WritePacket(append([]byte{0, 0, 0, 0}, packetBody...))
			})
			if err := relayConnectionPhase(pc, &testRelay{secure: true, reply: []byte{1}}); err == nil {
				t.Fatal("accepted a malformed packet")
			}
		})
	}
}

func TestRelayRejectsOversizedClientResponse(t *testing.T) {
	big := bytes.Repeat([]byte{1}, maxAuthResponseLen+1)
	for name, first := range map[string][]byte{
		"challenge": authSwitch("mysql_native_password", make([]byte, 20)),
		"more data": {0, 0, 0, 0, mysql.MORE_DATE_HEADER, mysql.CACHE_SHA2_FULL_AUTH},
	} {
		t.Run(name, func(t *testing.T) {
			pc := serveUpstream(t, func(pc *packet.Conn) {
				pc.WritePacket(first)
				pc.ReadPacket()
			})
			if err := relayConnectionPhase(pc, &testRelay{secure: true, reply: big}); err == nil {
				t.Fatal("forwarded an oversized client response")
			}
		})
	}
}

// The database's own rejection must come back as an unwrapped *mysql.MyError:
// go-mysql's writeError type-asserts on it, and auditingAuthHandler.OnAuthFailure
// reads the code from it to emit auth_fail.
func TestRelayReturnsUpstreamErrorUnwrapped(t *testing.T) {
	pc := serveUpstream(t, func(pc *packet.Conn) {
		pc.WritePacket(append([]byte{0, 0, 0, 0, mysql.ERR_HEADER, 0x15, 0x04}, []byte("#28000Access denied for user 'alice'")...))
	})
	err := relayConnectionPhase(pc, &testRelay{secure: true})
	myErr, ok := err.(*mysql.MyError)
	if !ok {
		t.Fatalf("error is %T, want a bare *mysql.MyError", err)
	}
	if myErr.Code != mysql.ER_ACCESS_DENIED_ERROR || myErr.State != "28000" {
		t.Fatalf("lost the database's code/state: %+v", myErr)
	}
}

func TestRelayPropagatesRelayErrors(t *testing.T) {
	boom := errors.New("client went away")
	pc := serveUpstream(t, func(pc *packet.Conn) {
		pc.WritePacket(authSwitch("mysql_native_password", make([]byte, 20)))
	})
	if err := relayConnectionPhase(pc, &failingRelay{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the relay's error", err)
	}
}

type failingRelay struct {
	testRelay
	err error
}

func (r *failingRelay) Challenge(string, []byte) ([]byte, error) { return nil, r.err }
