package p2p_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/internal/p2p"
	"github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/libs/utils/scope"
	"github.com/tendermint/tendermint/types"
)

// transportFactory is used to set up transports for tests.
type transportFactory = func(ctx context.Context) *p2p.Transport

// testTransports is a registry of transport factories for withTransports().
var testTransports = map[string](func() transportFactory){}

// withTransports is a test helper that runs a test against all transports
// registered in testTransports.
func withTransports(t *testing.T, tester func(*testing.T, transportFactory)) {
	t.Helper()
	for name, transportFactory := range testTransports {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(leaktest.Check(t))
			tester(t, transportFactory())
		})
	}
}

func TestTransport_DialEndpoints(t *testing.T) {
	ipTestCases := []struct {
		ip netip.Addr
		ok bool
	}{
		{netip.IPv4Unspecified(), true},
		{netip.IPv6Unspecified(), true},

		{netip.AddrFrom4([4]byte{255, 255, 255, 255}), false},
		{netip.AddrFrom4([4]byte{224, 0, 0, 1}), false},
	}

	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		endpoint := a.Endpoint()

		// Spawn a goroutine to simply accept any connections until closed.
		go func() {
			for {
				conn, err := a.Accept(ctx)
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		// Dialing self should work.
		conn, err := a.Dial(ctx, endpoint)
		require.NoError(t, err)
		require.NoError(t, conn.Close())

		// Dialing empty endpoint should error.
		_, err = a.Dial(ctx, p2p.Endpoint{})
		require.Error(t, err)

		// Tests for networked endpoints (with IP).
		for _, tc := range ipTestCases {
			t.Run(tc.ip.String(), func(t *testing.T) {
				e := endpoint
				e.AddrPort = netip.AddrPortFrom(tc.ip, endpoint.Port())
				conn, err := a.Dial(ctx, e)
				if tc.ok {
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				} else {
					require.Error(t, err, "endpoint=%s", e)
				}
			})
		}
	})
}

func TestTransport_Endpoints(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)

		// Both transports return valid and different endpoints.
		aEndpoint := a.Endpoint()
		bEndpoint := b.Endpoint()
		require.NotEqual(t, aEndpoint, bEndpoint)
		for _, endpoint := range []p2p.Endpoint{aEndpoint, bEndpoint} {
			err := endpoint.Validate()
			require.NoError(t, err, "invalid endpoint %q", endpoint)
		}
	})
}

func TestTransport_String(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		a := makeTransport(t.Context())
		require.NotEmpty(t, a.String())
	})
}

func TestConnection_Handshake(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)
		ab, ba := dialAccept(ctx, t, a, b)

		// A handshake should pass the given keys and NodeInfo.
		aKey := ed25519.GenPrivKey()
		aInfo := types.NodeInfo{
			NodeID: types.NodeIDFromPubKey(aKey.PubKey()),
			ProtocolVersion: types.ProtocolVersion{
				P2P:   1,
				Block: 2,
				App:   3,
			},
			ListenAddr: "127.0.0.1:1239",
			Network:    "network",
			Version:    "1.2.3",
			Channels:   bytes.HexBytes([]byte{0xf0, 0x0f}),
			Moniker:    "moniker",
			Other: types.NodeInfoOther{
				TxIndex:    "on",
				RPCAddress: "rpc.domain.com",
			},
		}
		bKey := ed25519.GenPrivKey()
		bInfo := types.NodeInfo{
			NodeID:     types.NodeIDFromPubKey(bKey.PubKey()),
			ListenAddr: "127.0.0.1:1234",
			Moniker:    "othermoniker",
			Other: types.NodeInfoOther{
				TxIndex: "off",
			},
		}

		errCh := make(chan error, 1)
		go func() {
			// Must use assert due to goroutine.
			peerInfo, err := ba.Handshake(ctx, bInfo, bKey)
			if err == nil {
				assert.Equal(t, aInfo, peerInfo)
			}
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}()

		peerInfo, err := ab.Handshake(ctx, aInfo, aKey)
		require.NoError(t, err)
		require.Equal(t, bInfo, peerInfo)

		require.NoError(t, <-errCh)
	})
}

func TestConnection_HandshakeCancel(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)

		// Handshake should error on context cancellation.
		ab, ba := dialAccept(ctx, t, a, b)
		timeoutCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		cancel()
		_, err := ab.Handshake(timeoutCtx, types.NodeInfo{}, ed25519.GenPrivKey())
		require.Error(t, err)
		_ = ab.Close()
		_ = ba.Close()

		// Handshake should error on context timeout.
		ab, ba = dialAccept(ctx, t, a, b)
		timeoutCtx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err = ab.Handshake(timeoutCtx, types.NodeInfo{}, ed25519.GenPrivKey())
		require.Error(t, err)
		_ = ab.Close()
		_ = ba.Close()
	})
}

func TestConnection_FlushClose(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)
		ab, _ := dialAcceptHandshake(ctx, t, a, b)

		ab.Close()

		_, _, err := ab.ReceiveMessage(ctx)
		require.Error(t, err)

		err = ab.SendMessage(ctx, chID, []byte("closed"))
		require.Error(t, err)
	})
}

func TestConnection_LocalRemoteEndpoint(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)
		ab, ba := dialAcceptHandshake(ctx, t, a, b)

		// Local and remote connection endpoints correspond to each other.
		require.NotEmpty(t, ab.LocalEndpoint())
		require.NotEmpty(t, ba.LocalEndpoint())
		require.Equal(t, ab.LocalEndpoint(), ba.RemoteEndpoint())
		require.Equal(t, ab.RemoteEndpoint(), ba.LocalEndpoint())
	})
}

func TestConnection_SendReceive(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)
		ab, ba := dialAcceptHandshake(ctx, t, a, b)

		// Can send and receive a to b.
		err := ab.SendMessage(ctx, chID, []byte("foo"))
		require.NoError(t, err)

		t.Logf("ba.ReceiveMessage")
		ch, msg, err := ba.ReceiveMessage(ctx)
		t.Logf("ba.ReceiveMessage returned")
		require.NoError(t, err)
		require.Equal(t, []byte("foo"), msg)
		require.Equal(t, chID, ch)

		// Can send and receive b to a.
		err = ba.SendMessage(ctx, chID, []byte("bar"))
		require.NoError(t, err)

		_, msg, err = ab.ReceiveMessage(ctx)
		require.NoError(t, err)
		require.Equal(t, []byte("bar"), msg)

		// Close one side of the connection. Both sides should then error
		// with io.EOF when trying to send or receive.
		ba.Close()

		_, _, err = ab.ReceiveMessage(ctx)
		t.Logf("errrr = %v", err)
		require.Error(t, err)

		err = ab.SendMessage(ctx, chID, []byte("closed"))
		require.Error(t, err)

		_, _, err = ba.ReceiveMessage(ctx)
		require.Error(t, err)

		err = ba.SendMessage(ctx, chID, []byte("closed"))
		require.Error(t, err)
	})
}

func TestConnection_String(t *testing.T) {
	withTransports(t, func(t *testing.T, makeTransport transportFactory) {
		ctx := t.Context()
		a := makeTransport(ctx)
		b := makeTransport(ctx)
		ab, _ := dialAccept(ctx, t, a, b)
		require.NotEmpty(t, ab.String())
	})
}

func TestEndpoint_NodeAddress(t *testing.T) {
	var (
		ip4 = netip.AddrFrom4([4]byte{1, 2, 3, 4})
		ip6 = netip.AddrFrom16([16]byte{0xb1, 0x0c, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01})
		id  = types.NodeID("00112233445566778899aabbccddeeff00112233")
	)

	testcases := []struct {
		endpoint p2p.Endpoint
		expect   p2p.NodeAddress
	}{
		// Valid endpoints.
		{
			p2p.Endpoint{netip.AddrPortFrom(ip4, 8080)},
			p2p.NodeAddress{Hostname: "1.2.3.4", Port: 8080},
		},
		{
			p2p.Endpoint{netip.AddrPortFrom(ip6, 8080)},
			p2p.NodeAddress{Hostname: "b10c::1", Port: 8080},
		},

		// Partial (invalid) endpoints.
		{p2p.Endpoint{}, p2p.NodeAddress{}},
		{p2p.Endpoint{netip.AddrPortFrom(ip4, 0)}, p2p.NodeAddress{Hostname: "1.2.3.4"}},
	}
	for _, tc := range testcases {
		t.Run(tc.endpoint.String(), func(t *testing.T) {
			// Without NodeID.
			expect := tc.expect
			require.Equal(t, expect, tc.endpoint.NodeAddress(""))

			// With NodeID.
			expect.NodeID = id
			require.Equal(t, expect, tc.endpoint.NodeAddress(expect.NodeID))
		})
	}
}

func TestEndpoint_Validate(t *testing.T) {
	ip4 := netip.AddrFrom4([4]byte{1, 2, 3, 4})
	ip6 := netip.AddrFrom16([16]byte{0xb1, 0x0c, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01})

	testcases := []struct {
		endpoint    p2p.Endpoint
		expectValid bool
	}{
		// Valid endpoints.
		{p2p.Endpoint{netip.AddrPortFrom(ip4, 0)}, true},
		{p2p.Endpoint{netip.AddrPortFrom(ip6, 0)}, true},
		{p2p.Endpoint{netip.AddrPortFrom(ip4, 8008)}, true},

		// Invalid endpoints.
		{p2p.Endpoint{}, false},
		{p2p.Endpoint{netip.AddrPortFrom(ip4, 0)}, false},
	}
	for _, tc := range testcases {
		t.Run(tc.endpoint.String(), func(t *testing.T) {
			err := tc.endpoint.Validate()
			if tc.expectValid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// dialAccept is a helper that dials b from a and returns both sides of the
// connection.
func dialAccept(ctx context.Context, t *testing.T, a, b *p2p.Transport) (p2p.Connection, p2p.Connection) {
	defer t.Logf("dialAccept DONE")
	t.Helper()

	endpoint := b.Endpoint()

	var acceptConn p2p.Connection
	var dialConn p2p.Connection
	if err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.Spawn(func() error {
			var err error
			if dialConn, err = a.Dial(ctx, endpoint); err != nil {
				return err
			}
			t.Cleanup(func() { _ = dialConn.Close() })
			return nil
		})
		var err error
		if acceptConn, err = b.Accept(ctx); err != nil {
			return err
		}
		t.Cleanup(func() { _ = acceptConn.Close() })
		return nil
	}); err != nil {
		t.Fatalf("dial/accept failed: %v", err)
	}
	return dialConn, acceptConn
}

// dialAcceptHandshake is a helper that dials and handshakes b from a and
// returns both sides of the connection.
func dialAcceptHandshake(ctx context.Context, t *testing.T, a, b *p2p.Transport) (p2p.Connection, p2p.Connection) {
	defer t.Logf("dialAcceptHandshake DONE")
	t.Helper()

	ab, ba := dialAccept(ctx, t, a, b)

	err := scope.Run(ctx, func(ctx context.Context, s scope.Scope) error {
		s.Spawn(func() error {
			privKey := ed25519.GenPrivKey()
			nodeInfo := types.NodeInfo{
				NodeID:     types.NodeIDFromPubKey(privKey.PubKey()),
				ListenAddr: "127.0.0.1:1235",
				Moniker:    "a",
			}
			_, err := ba.Handshake(ctx, nodeInfo, privKey)
			return err
		})
		privKey := ed25519.GenPrivKey()
		nodeInfo := types.NodeInfo{
			NodeID:     types.NodeIDFromPubKey(privKey.PubKey()),
			ListenAddr: "127.0.0.1:1234",
			Moniker:    "b",
		}
		_, err := ab.Handshake(ctx, nodeInfo, privKey)
		return err
	})

	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	return ab, ba
}
