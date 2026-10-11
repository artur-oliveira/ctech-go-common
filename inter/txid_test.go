package inter

import (
	"strings"
	"testing"
)

func TestTxidFollowsBacen(t *testing.T) {
	if !ValidTxid("ctb01JABCDEFGHJKMNPQRSTVWXYZ") { // 29 chars
		t.Fatal("billing's txid shape refused")
	}
	for _, bad := range []string{"short", strings.Repeat("a", 36), "ctb-01JABCDEFGHJKMNPQRSTVWXY"} {
		if ValidTxid(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}
