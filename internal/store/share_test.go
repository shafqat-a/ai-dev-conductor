package store

import (
	"crypto/sha256"
	"testing"
)

func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func TestMintAndRedeemShare(t *testing.T) {
	st := openTemp(t)
	h := hash("secret-token")
	if err := st.MintShare("sh1", h, "sess-a", "read", 1000, 2000); err != nil {
		t.Fatalf("MintShare: %v", err)
	}

	sid, mode, ok, err := st.RedeemShare(h, 1500)
	if err != nil {
		t.Fatalf("RedeemShare: %v", err)
	}
	if !ok || sid != "sess-a" || mode != "read" {
		t.Fatalf("redeem mismatch: ok=%v sid=%q mode=%q", ok, sid, mode)
	}
}

func TestRedeemExpired(t *testing.T) {
	st := openTemp(t)
	h := hash("t")
	st.MintShare("sh1", h, "sess-a", "read", 1000, 2000)

	_, _, ok, err := st.RedeemShare(h, 2001) // after expiry
	if err != nil {
		t.Fatalf("RedeemShare: %v", err)
	}
	if ok {
		t.Fatal("expired link must not redeem")
	}
}

func TestRedeemRevoked(t *testing.T) {
	st := openTemp(t)
	h := hash("t")
	st.MintShare("sh1", h, "sess-a", "read", 1000, 9999)
	if err := st.RevokeShare("sh1"); err != nil {
		t.Fatalf("RevokeShare: %v", err)
	}

	_, _, ok, _ := st.RedeemShare(h, 1500)
	if ok {
		t.Fatal("revoked link must not redeem")
	}
}

func TestRedeemUnknown(t *testing.T) {
	st := openTemp(t)
	_, _, ok, err := st.RedeemShare(hash("nope"), 1500)
	if err != nil {
		t.Fatalf("RedeemShare: %v", err)
	}
	if ok {
		t.Fatal("unknown token must not redeem")
	}
}

func TestSharesForSession(t *testing.T) {
	st := openTemp(t)
	st.MintShare("sh1", hash("a"), "sess-a", "read", 1000, 9999)
	st.MintShare("sh2", hash("b"), "sess-a", "read", 2000, 9999)
	st.MintShare("sh3", hash("c"), "sess-b", "read", 1500, 9999)

	got, err := st.SharesForSession("sess-a")
	if err != nil {
		t.Fatalf("SharesForSession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 shares for sess-a, got %d", len(got))
	}
	// Newest first (created_at DESC).
	if got[0].ID != "sh2" || got[1].ID != "sh1" {
		t.Fatalf("unexpected order: %q, %q", got[0].ID, got[1].ID)
	}
	if got[0].SessionID != "sess-a" || got[0].Mode != "read" || got[0].Revoked {
		t.Fatalf("unexpected share: %+v", got[0])
	}
}
