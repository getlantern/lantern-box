//go:build with_quic

package unbounded

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/tls"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"
)

// quicV1InitialSalt is RFC 9001 §5.2's.
var quicV1InitialSalt, _ = hex.DecodeString("38762cf7f55934b34d179ae6a4c80cadccbb7f0a")

func hkdfExpandLabel(secret []byte, label string, n int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 4+len(full))
	info = binary.BigEndian.AppendUint16(info, uint16(n))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out, err := hkdf.Expand(sha256.New, secret, string(info), n)
	if err != nil {
		panic(err)
	}
	return out
}

func quicVarint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errors.New("short varint")
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, errors.New("short varint")
	}
	v := uint64(b[0] & 0x3f)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n, nil
}

// decryptQUICv1Initial removes a client QUIC v1 Initial's protection, with the keys anyone on
// the path can derive from its Destination Connection ID, as a DPI box does to read the SNI.
// It returns the packet's plaintext payload: the CRYPTO frames carrying the ClientHello.
func decryptQUICv1Initial(pkt []byte) ([]byte, error) {
	if !isQUICv1LongHeader(pkt) || pkt[0]&0x30 != 0 {
		return nil, errors.New("not a QUIC v1 Initial")
	}
	p := append([]byte(nil), pkt...)
	off := 5
	if off >= len(p) {
		return nil, errors.New("short")
	}
	dcidLen := int(p[off])
	off++
	if off+dcidLen >= len(p) {
		return nil, errors.New("short")
	}
	dcid := p[off : off+dcidLen]
	off += dcidLen
	off += 1 + int(p[off]) // SCID
	tokenLen, n, err := quicVarint(p[min(off, len(p)):])
	if err != nil {
		return nil, err
	}
	off += n + int(tokenLen)
	length, n, err := quicVarint(p[min(off, len(p)):])
	if err != nil {
		return nil, err
	}
	pnOff := off + n
	end := pnOff + int(length)
	if pnOff+20 > len(p) || end > len(p) {
		return nil, errors.New("short")
	}

	initial, err := hkdf.Extract(sha256.New, dcid, quicV1InitialSalt)
	if err != nil {
		return nil, err
	}
	client := hkdfExpandLabel(initial, "client in", 32)
	key := hkdfExpandLabel(client, "quic key", 16)
	iv := hkdfExpandLabel(client, "quic iv", 12)
	hp := hkdfExpandLabel(client, "quic hp", 16)

	hpBlock, err := aes.NewCipher(hp)
	if err != nil {
		return nil, err
	}
	mask := make([]byte, 16)
	hpBlock.Encrypt(mask, p[pnOff+4:pnOff+20])
	p[0] ^= mask[0] & 0x0f
	pnLen := int(p[0]&0x03) + 1
	var pn uint64
	for i := range pnLen {
		p[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(p[pnOff+i])
	}
	nonce := append([]byte(nil), iv...)
	for i := range 8 {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, p[pnOff+pnLen:end], p[:pnOff+pnLen])
}

// initialCryptoStream decrypts every client QUIC v1 Initial among datagrams and reassembles
// the CRYPTO stream they carry, from offset 0 for as far as it runs unbroken: the ClientHello,
// which quic-go splits across frames. Datagrams that aren't Initials are skipped.
func initialCryptoStream(datagrams [][]byte) []byte {
	chunks := map[uint64][]byte{}
	for _, d := range datagrams {
		plain, err := decryptQUICv1Initial(d)
		if err != nil {
			continue
		}
		for len(plain) > 0 {
			switch plain[0] {
			case 0x00, 0x01: // PADDING, PING
				plain = plain[1:]
				continue
			case 0x06: // CRYPTO
			default:
				plain = nil
				continue
			}
			off, n, err := quicVarint(plain[1:])
			if err != nil {
				break
			}
			l, m, err := quicVarint(plain[1+n:])
			if err != nil || 1+n+m+int(l) > len(plain) {
				break
			}
			chunks[off] = plain[1+n+m : 1+n+m+int(l)]
			plain = plain[1+n+m+int(l):]
		}
	}
	var stream []byte
	for {
		next, ok := chunks[uint64(len(stream))]
		if !ok || len(next) == 0 {
			return stream
		}
		stream = append(stream, next...)
	}
}

// The control for TestExitPathsObfuscateTheWire's SNI check: a stock Hysteria2 client's
// Initial, as a v0.0.239 consumer sent, gives up its server name to decryptQUICv1Initial.
func TestDecryptQUICv1InitialReadsAStockClientsSNI(t *testing.T) {
	vConn, uConn := pathPair(t)
	const serverName = "www.example.com"
	tlsConfig, err := tls.NewClient(context.Background(), testLogger(), serverName, sbOption.OutboundTLSOptions{
		Enabled:    true,
		ServerName: serverName,
		ALPN:       badoption.Listable[string]{"h3"},
	})
	require.NoError(t, err)
	c, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       context.Background(),
		Dialer:        &pathDialer{conn: &packetConnAsConn{PacketConn: uConn}},
		Logger:        testLogger(),
		ServerAddress: M.ParseSocksaddrHostPort(serverName, 443),
		Password:      "pw",
		TLSConfig:     tlsConfig,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = c.DialConn(ctx, M.ParseSocksaddr("example.com:80")) }()
	t.Cleanup(func() { _ = c.CloseWithError(errors.New("test over")) })

	_ = vConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var wire [][]byte
	b := make([]byte, 2048)
	for {
		n, _, err := vConn.ReadFrom(b)
		require.NoError(t, err, "no ClientHello arrived")
		wire = append(wire, append([]byte(nil), b[:n]...))
		if strings.Contains(string(initialCryptoStream(wire)), serverName) {
			return
		}
	}
}
