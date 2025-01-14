package libp2ptls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"

	"golang.org/x/sys/cpu"

	ic "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
)

const certValidityPeriod = 100 * 365 * 24 * time.Hour // ~100 years
const certificatePrefix = "libp2p-tls-handshake:"
const alpn string = "libp2p"

var extensionID = getPrefixedExtensionID([]int{1, 1})
var extensionCritical bool // so we can mark the extension critical in tests

type signedKey struct {
	PubKey    []byte
	Signature []byte
}

// Identity is used to secure connections
type Identity struct {
	config tls.Config
}

// NewIdentity creates a new identity
func NewIdentity(privKey ic.PrivKey) (*Identity, error) {
	if caEnable {
		return &Identity{config: *caConfig.tlsConfig.Clone()}, nil
	}
	cert, err := keyToCertificate(privKey)
	if err != nil {
		return nil, err
	}
	return &Identity{
		config: tls.Config{
			MinVersion:               tls.VersionTLS13,
			PreferServerCipherSuites: preferServerCipherSuites(),
			InsecureSkipVerify:       true, // This is not insecure here. We will verify the cert chain ourselves.
			ClientAuth:               tls.RequireAnyClientCert,
			Certificates:             []tls.Certificate{*cert},
			VerifyPeerCertificate: func(_ [][]byte, _ [][]*x509.Certificate) error {
				panic("tls config not specialized for peer")
			},
			NextProtos:             []string{alpn},
			SessionTicketsDisabled: true,
		},
	}, nil
}

// ConfigForPeer creates a new single-use tls.Config that verifies the peer's
// certificate chain and returns the peer's public key via the channel. If the
// peer ID is empty, the returned config will accept any peer.
//
// It should be used to create a new tls.Config before securing either an
// incoming or outgoing connection.
func (i *Identity) ConfigForPeer(remote peer.ID) (*tls.Config, <-chan ic.PubKey) {
	keyCh := make(chan ic.PubKey, 1)
	// We need to check the peer ID in the VerifyPeerCertificate callback.
	// The tls.Config it is also used for listening, and we might also have concurrent dials.
	// Clone it so we can check for the specific peer ID we're dialing here.
	conf := i.config.Clone()
	// We're using InsecureSkipVerify, so the verifiedChains parameter will always be empty.
	// We need to parse the certificates ourselves from the raw certs.
	conf.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		defer close(keyCh)

		chain := make([]*x509.Certificate, len(rawCerts))
		for i := 0; i < len(rawCerts); i++ {
			cert, err := x509.ParseCertificate(rawCerts[i])
			if err != nil {
				return err
			}
			chain[i] = cert
		}

		pubKey, err := PubKeyFromCertChain(chain)
		if err != nil {
			return err
		}
		if remote != "" && !remote.MatchesPublicKey(pubKey) {
			peerID, err := peer.IDFromPublicKey(pubKey)
			if err != nil {
				peerID = peer.ID(fmt.Sprintf("(not determined: %s)", err.Error()))
			}
			return fmt.Errorf("peer IDs don't match: expected %s, got %s", remote, peerID)
		}
		keyCh <- pubKey
		return nil
	}
	return conf, keyCh
}

// PubKeyFromCertChain verifies the certificate chain and extract the remote's public key.
func PubKeyFromCertChain(chain []*x509.Certificate) (ic.PubKey, error) {
	if len(chain) != 1 {
		return nil, errors.New("expected one certificates in the chain")
	}
	if caEnable {
		return pubKeyFromCertChainAndTLSConfig(chain)
	}
	cert := chain[0]
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	var found bool
	var keyExt pkix.Extension
	// find the libp2p key extension, skipping all unknown extensions
	for _, ext := range cert.Extensions {
		if extensionIDEqual(ext.Id, extensionID) {
			keyExt = ext
			found = true
			for i, oident := range cert.UnhandledCriticalExtensions {
				if oident.Equal(ext.Id) {
					// delete the extension from UnhandledCriticalExtensions
					cert.UnhandledCriticalExtensions = append(cert.UnhandledCriticalExtensions[:i], cert.UnhandledCriticalExtensions[i+1:]...)
					break
				}
			}
			break
		}
	}
	if !found {
		return nil, errors.New("expected certificate to contain the key extension")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		// If we return an x509 error here, it will be sent on the wire.
		// Wrap the error to avoid that.
		return nil, fmt.Errorf("certificate verification failed: %s", err)
	}

	var sk signedKey
	if _, err := asn1.Unmarshal(keyExt.Value, &sk); err != nil {
		return nil, fmt.Errorf("unmarshalling signed certificate failed: %s", err)
	}
	pubKey, err := ic.UnmarshalPublicKey(sk.PubKey)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling public key failed: %s", err)
	}
	certKeyPub, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return nil, err
	}
	valid, err := pubKey.Verify(append([]byte(certificatePrefix), certKeyPub...), sk.Signature)
	if err != nil {
		return nil, fmt.Errorf("signature verification failed: %s", err)
	}
	if !valid {
		return nil, errors.New("signature invalid")
	}
	return pubKey, nil
}

func keyToCertificate(sk ic.PrivKey) (*tls.Certificate, error) {
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	keyBytes, err := ic.MarshalPublicKey(sk.GetPublic())
	if err != nil {
		return nil, err
	}
	certKeyPub, err := x509.MarshalPKIXPublicKey(certKey.Public())
	if err != nil {
		return nil, err
	}
	signature, err := sk.Sign(append([]byte(certificatePrefix), certKeyPub...))
	if err != nil {
		return nil, err
	}
	value, err := asn1.Marshal(signedKey{
		PubKey:    keyBytes,
		Signature: signature,
	})
	if err != nil {
		return nil, err
	}

	bigNum := big.NewInt(1 << 62)
	sn, err := rand.Int(rand.Reader, bigNum)
	if err != nil {
		return nil, err
	}
	subjectSN, err := rand.Int(rand.Reader, bigNum)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certValidityPeriod),
		// According to RFC 3280, the issuer field must be set,
		// see https://datatracker.ietf.org/doc/html/rfc3280#section-4.1.2.4.
		Subject: pkix.Name{SerialNumber: subjectSN.String()},
		// after calling CreateCertificate, these will end up in Certificate.Extensions
		ExtraExtensions: []pkix.Extension{
			{Id: extensionID, Critical: extensionCritical, Value: value},
		},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, certKey.Public(), certKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  certKey,
	}, nil
}

// We want nodes without AES hardware (e.g. ARM) support to always use ChaCha.
// Only if both nodes have AES hardware support (e.g. x86), AES should be used.
// x86->x86: AES, ARM->x86: ChaCha, x86->ARM: ChaCha and ARM->ARM: Chacha
// This function returns true if we don't have AES hardware support, and false otherwise.
// Thus, ARM servers will always use their own cipher suite preferences (ChaCha first),
// and x86 servers will aways use the client's cipher suite preferences.
func preferServerCipherSuites() bool {
	// Copied from the Go TLS implementation.

	// Check the cpu flags for each platform that has optimized GCM implementations.
	// Worst case, these variables will just all be false.
	var (
		hasGCMAsmAMD64 = cpu.X86.HasAES && cpu.X86.HasPCLMULQDQ
		hasGCMAsmARM64 = cpu.ARM64.HasAES && cpu.ARM64.HasPMULL
		// Keep in sync with crypto/aes/cipher_s390x.go.
		hasGCMAsmS390X = cpu.S390X.HasAES && cpu.S390X.HasAESCBC && cpu.S390X.HasAESCTR && (cpu.S390X.HasGHASH || cpu.S390X.HasAESGCM)

		hasGCMAsm = hasGCMAsmAMD64 || hasGCMAsmARM64 || hasGCMAsmS390X
	)
	return !hasGCMAsm
}
