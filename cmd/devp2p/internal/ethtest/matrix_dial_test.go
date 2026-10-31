package ethtest

import (
	"fmt"
	"os"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/crypto"
	"github.com/XinFinOrg/XDPoSChain/p2p"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
)

// TestMatrixPinnedVersionHandshake dials the live devnet node under
// MATRIX_ENODE advertising a single pinned eth capability and asserts the
// status exchange settles at exactly that version. This is the §8.3 matrix's
// handshake-level leg: real node pairs cannot produce xdc/164 or xdc/100
// against an XDC166 peer (negotiation takes the highest shared version), so
// the pinned dial is the honest coverage for those two pairs. Skips unless
// MATRIX_ENODE and MATRIX_CHAINDIR are set.
func TestMatrixPinnedVersionHandshake(t *testing.T) {
	enodeStr := os.Getenv("MATRIX_ENODE")
	chainDir := os.Getenv("MATRIX_CHAINDIR")
	if enodeStr == "" || chainDir == "" {
		t.Skip("MATRIX_ENODE/MATRIX_CHAINDIR not set (devnet matrix only)")
	}
	node, err := enode.Parse(enode.ValidSchemes, enodeStr)
	if err != nil {
		t.Fatalf("bad MATRIX_ENODE: %v", err)
	}
	suite, err := NewSuite(node, chainDir, "", "")
	if err != nil {
		t.Fatalf("failed to load chain dir %s: %v", chainDir, err)
	}
	for _, version := range []uint{ethProtoVersionXDpos164, ethProtoVersionXDpos2, ethProtoVersionXDpos166} {
		t.Run(fmt.Sprintf("xdc%d", version), func(t *testing.T) {
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatalf("generate key: %v", err)
			}
			conn, err := suite.dialAs(key)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			// Pin: advertise only this version so the negotiation cannot
			// settle anywhere higher.
			conn.caps = []p2p.Cap{{Name: "eth", Version: version}}
			conn.ourHighestProtoVersion = version
			if err := conn.statusExchange(suite.chain, nil); err != nil {
				t.Fatalf("status exchange at pinned xdc/%d failed: %v", version, err)
			}
			if conn.negotiatedProtoVersion != version {
				t.Fatalf("negotiated xdc/%d, want xdc/%d", conn.negotiatedProtoVersion, version)
			}
		})
	}
}
