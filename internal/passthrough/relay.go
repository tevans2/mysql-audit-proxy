// Package passthrough contains the COM_CHANGE_USER route from the working
// origin/53-spike-com-change-user prototype, adapted for a TLS-only upstream.
// Production pass-through authentication will supply the client-side relay.
package passthrough

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/packet"
)

// AuthRelay forwards authentication bytes on a TLS client connection. Callers
// must enforce client TLS before relaying password-bearing plugins, and bound
// all client reads. Authentication payloads must never be logged or retained.
type AuthRelay interface {
	ClientOnTLS() bool
	Challenge(plugin string, salt []byte) ([]byte, error)
	Notify(payload []byte) error
	Relay(payload []byte) ([]byte, error)
	Log(message string)
}

// Bounds on the upstream connection phase. A real exchange is two or three
// round trips (auth switch, then at most a request for full authentication);
// the limits only exist so a broken or hostile upstream cannot keep the
// session in this loop, or make the proxy buffer an unbounded reply.
const (
	// maxAuthRounds is how many packets the upstream may send before it must
	// have answered OK or ERR.
	maxAuthRounds = 8
	// maxPluginNameLen is the longest plugin name accepted in an auth switch.
	maxPluginNameLen = 255
	// maxAuthChallengeLen is the longest salt or more-data payload accepted
	// from the upstream.
	maxAuthChallengeLen = 4096
	// maxAuthResponseLen is the longest reply the client relay may hand back.
	// RSA-encrypted passwords are at most 512 bytes.
	maxAuthResponseLen = 16384
)

func relayConnectionPhase(pc *packet.Conn, relay AuthRelay) error {
	for round := 0; ; round++ {
		if round >= maxAuthRounds {
			return fmt.Errorf("upstream did not finish authentication within %d packets", maxAuthRounds)
		}
		data, err := pc.ReadPacket()
		if err != nil {
			return fmt.Errorf("read upstream auth reply: %w", err)
		}
		if len(data) == 0 {
			return errors.New("empty packet during upstream connection phase")
		}

		switch data[0] {
		case mysql.OK_HEADER:
			return nil

		case mysql.ERR_HEADER:
			return parseErrPacket(data)

		case mysql.EOF_HEADER:
			// AuthSwitchRequest: plugin name, NUL, then the new salt.
			idx := bytes.IndexByte(data[1:], 0x00)
			if idx < 0 {
				return errors.New("malformed upstream auth switch request")
			}
			if idx == 0 || idx > maxPluginNameLen {
				return errors.New("upstream auth switch names an invalid plugin")
			}
			plugin := string(data[1 : 1+idx])
			salt := bytes.TrimSuffix(data[1+idx+1:], []byte{0})
			if len(salt) > maxAuthChallengeLen {
				return errors.New("upstream auth switch challenge is too large")
			}

			resp, err := relay.Challenge(plugin, salt)
			if err != nil {
				return err
			}
			if len(resp) > maxAuthResponseLen {
				return errors.New("client authentication response is too large")
			}
			if err := pc.WritePacket(append(make([]byte, 4), resp...)); err != nil {
				return fmt.Errorf("write upstream auth switch response: %w", err)
			}

		case mysql.MORE_DATE_HEADER:
			payload := data[1:]
			if len(payload) > maxAuthChallengeLen {
				return errors.New("upstream auth data is too large")
			}

			// caching_sha2_password's fast-auth signal. The client is waiting
			// for it before it will accept an OK, and it needs no reply; the
			// upstream's own OK follows on the next iteration.
			if len(payload) == 1 && payload[0] == mysql.CACHE_SHA2_FAST_AUTH {
				relay.Log("upstream accepted caching_sha2_password FAST auth (cache hit)")
				if err := relay.Notify(payload); err != nil {
					return err
				}
				continue
			}

			// Both legs must be TLS for password-bearing plugins. Forward raw
			// responses without converting them to strings or re-encrypting.
			resp, err := relay.Relay(payload)
			if err != nil {
				return err
			}
			if len(resp) > maxAuthResponseLen {
				return errors.New("client authentication response is too large")
			}
			if err := pc.WritePacket(append(make([]byte, 4), resp...)); err != nil {
				return fmt.Errorf("write upstream auth more data: %w", err)
			}

		default:
			return fmt.Errorf("unexpected packet header %#x during upstream connection phase", data[0])
		}
	}
}

// parseErrPacket turns an upstream ERR packet into a *mysql.MyError, so the
// real error code and message survive all the way to the client and the audit
// log. Do not wrap the result: go-mysql's writeError type-asserts on
// *mysql.MyError rather than using errors.As (see docs/known-limitations.md).
func parseErrPacket(data []byte) *mysql.MyError {
	e := new(mysql.MyError)
	pos := 1
	if len(data) < 3 {
		e.Code = mysql.ER_UNKNOWN_ERROR
		e.Message = "malformed error packet from upstream"
		return e
	}
	e.Code = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2
	if len(data) > pos && data[pos] == '#' {
		pos++
		if len(data) >= pos+5 {
			e.State = string(data[pos : pos+5])
			pos += 5
		}
	}
	if len(data) > pos {
		e.Message = string(data[pos:])
	}
	return e
}
