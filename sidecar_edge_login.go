//go:build !ios_extension && !js

// SignEdgeLogin (§18.5, §13.7's members-device audience, R-SDK-12, I-4; SDK-M6-03): the member's device vouches for
// one edge login. A members-device host's front, the member's ID token checked, issues a nonce at its origin and asks
// for the device's statement; the app or extension hands both here, the core signs §6's SIGNED_TLS_EDGE_LOGIN over
// exactly that origin and nonce under the device's identity key (Sidecar.SignTlsEdgeLogin, TLS-M6-06), and the result
// is the statement for the visitor's browser to post back.
//
//	binds      the statement names the origin and the nonce and nothing else, so it completes that one login at that
//	           one origin: a front refuses it at every other origin and for every other nonce
//	origin     the caller passes the origin of the page that asked, as the browser or the OS reports it, never one the
//	           page names for itself; it goes to the core as given, which signs only https://<a canonical host>, with
//	           a port other than 443 named
//	consent    only a member's consented device signs: the core refuses on a tagged, ephemeral, parked or read_only
//	           sidecar, before enrollment, and while it holds no verified state of its network, and nothing here
//	           signs in its place
//	remote     a remote-mode Sidecar's device is its daemon, so the call goes as §5.8's tls.edge_login.sign (QD-827)
//	           on the paired network, with the nonce's bytes decoded here as for a local core. It is sent confirmed,
//	           because the bound call is the person's act in the app's UI, as AnnounceRepo and RevokeRepo go
//	           (D-CORE-13): an app calls it only for a login the person asked for there. The daemon signs only for a
//	           confirmed control call, under the same consent rule, and what it answers is taken only as a Signed of
//	           SIGNED_TLS_EDGE_LOGIN naming the origin and the nonce sent; its signature is the front's to check
//
// The nonce and the statement are one login's secrets: nothing here logs either, and a refusal's message repeats
// neither (I-7), a daemon's refusal that would being replaced. Safe for concurrent use.

package sdk

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// What SignEdgeLogin holds its arguments to.
type sidecarEdgeLoginSettings struct {
	// The longest nonce text read: 128 B, several times a front's 32-byte nonce in base64. A longer one is refused
	// before it is decoded.
	NonceTextMaxByteCount int
}

// The defaults, a fresh value each call.
func defaultSidecarEdgeLoginSettings() *sidecarEdgeLoginSettings {
	return &sidecarEdgeLoginSettings{
		NonceTextMaxByteCount: 128,
	}
}

// tls.edge_login.sign, as TLS named it (QD-827); sidecar_edge_login_test.go holds it and the shapes below equal to
// the daemon's.
const sidecarControlMethodTlsEdgeLoginSign = "tls.edge_login.sign"

// tls.edge_login.sign's params: the paired network, the origin as given and the nonce's bytes.
type sidecarControlTlsEdgeLoginSignParams struct {
	Network string `json:"network,omitempty"`
	Origin  string `json:"origin"`
	Nonce   []byte `json:"nonce"`
}

// tls.edge_login.sign's reply: the encoded Signed.
type sidecarControlTlsEdgeLoginSignReply struct {
	Statement []byte `json:"statement"`
}

var (
	// The nonce is not base64, or is longer than any a front issues.
	errSidecarEdgeLoginNonce = errors.New("an edge login's nonce is the front's, in base64")
	// The daemon answered something other than the statement asked for.
	errSidecarEdgeLoginReply = errors.New("the daemon's answer is not an edge login statement for that origin and nonce")
	// The daemon refused in a message that repeated the nonce.
	errSidecarEdgeLoginRefused = errors.New("the daemon refused the edge login")
	// The Sidecar closed.
	errSidecarEdgeLoginClosed = errors.New("the sidecar is closed")
	// A Sidecar with neither a core nor a daemon: never opened.
	errSidecarEdgeLoginNoBackend = errors.New("the sidecar has neither a core nor a daemon to sign with")
)

// The bytes text names: base64url as a front's challenge writes its nonce, padded or not, or standard base64.
func sidecarEdgeLoginNonceOf(text string, settings *sidecarEdgeLoginSettings) ([]byte, error) {
	if text == "" || settings.NonceTextMaxByteCount < len(text) {
		return nil, errSidecarEdgeLoginNonce
	}
	unpadded := strings.TrimRight(text, "=")
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		if nonce, err := encoding.Strict().DecodeString(unpadded); err == nil {
			return nonce, nil
		}
	}
	return nil, errSidecarEdgeLoginNonce
}

// Signs this device's statement for the members-device login a front issued the nonce nonceBase64 for at origin
// (§13.7). The result's Value is the statement, the base64url of the encoded Signed, which the visitor's browser
// posts as the device step's s. origin is the origin of the page that asked, never one the page names for itself.
// Only a member's consented device signs; every other answers a refusal, with nothing signed. On a remote-mode
// Sidecar the daemon signs, taking this call as the person's confirmed act (D-CORE-13), so an app calls it only for a
// login the person asked for in its UI.
func (self *Sidecar) SignEdgeLogin(origin string, nonceBase64 string, callback SidecarResultCallback) {
	go func() {
		result, err := traceWithReturnError(func() (*SidecarResult, error) {
			statement, err := self.signEdgeLogin(origin, nonceBase64)
			return sidecarResultOf(statement, err), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// SignEdgeLogin's statement, or why none was signed.
func (self *Sidecar) signEdgeLogin(origin string, nonceBase64 string) (string, error) {
	local, remote := self.backends()
	settings, closed := func() (*sidecarEdgeLoginSettings, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.edgeLoginSettings, self.closed
	}()
	if closed {
		return "", errSidecarEdgeLoginClosed
	}
	if settings == nil {
		settings = defaultSidecarEdgeLoginSettings()
	}
	nonce, err := sidecarEdgeLoginNonceOf(nonceBase64, settings)
	if err != nil {
		return "", err
	}
	var signed []byte
	switch {
	case local != nil:
		// the core checks the origin and the nonce's length, and that this device is its member's consented one
		signed, err = local.SignTlsEdgeLogin(origin, nonce)
	case remote != nil:
		// the daemon's core checks the same, for a confirmed call alone
		signed, err = self.signEdgeLoginRemote(remote, origin, nonce, nonceBase64)
	default:
		err = errSidecarEdgeLoginNoBackend
	}
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(signed), nil
}

// The daemon's statement for origin and nonce, through tls.edge_login.sign on the paired network and sent confirmed
// (see the file header). A refusal whose text repeats the nonce is replaced, and an answer that is not the statement
// asked for is refused.
func (self *Sidecar) signEdgeLoginRemote(remote sidecarBackend, origin string, nonce []byte, nonceText string) ([]byte, error) {
	params := &sidecarControlTlsEdgeLoginSignParams{Origin: origin, Nonce: nonce}
	if named, ok := remote.(interface{ network() string }); ok {
		params.Network = named.network()
	}
	reply := &sidecarControlTlsEdgeLoginSignReply{}
	if err := remote.call(self.ctx, sidecarControlMethodTlsEdgeLoginSign, params, true, reply); err != nil {
		message := err.Error()
		for _, held := range []string{nonceText, base64.StdEncoding.EncodeToString(nonce), base64.RawURLEncoding.EncodeToString(nonce)} {
			if held != "" && strings.Contains(message, held) {
				return nil, errSidecarEdgeLoginRefused
			}
		}
		return nil, err
	}
	if !sidecarEdgeLoginStatementNames(reply.Statement, origin, nonce) {
		return nil, errSidecarEdgeLoginReply
	}
	return reply.Statement, nil
}

// Whether statement is an encoded Signed of SIGNED_TLS_EDGE_LOGIN, with a signer and a signature, whose body names
// origin and nonce: the statement asked for. The signature is the front's to verify, under the signer's identity key in
// its config.
func sidecarEdgeLoginStatementNames(statement []byte, origin string, nonce []byte) bool {
	signed := &bridge.Signed{}
	if len(statement) == 0 || proto.Unmarshal(statement, signed) != nil {
		return false
	}
	if signed.GetType() != uint32(bridge.SignedType_SIGNED_TLS_EDGE_LOGIN) || len(signed.GetSignerId()) == 0 || len(signed.GetSignature()) == 0 {
		return false
	}
	login := &protocol.TlsEdgeLogin{}
	if proto.Unmarshal(signed.GetBody(), login) != nil {
		return false
	}
	return login.GetOrigin() == origin && bytes.Equal(login.GetNonce(), nonce)
}
