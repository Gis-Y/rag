package token

import (
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestTokenUseIsEnforced(t *testing.T) {
	manager := NewJWTManager("test-secret", 1, 1)
	access, err := manager.GenerateToken(1, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := manager.GenerateRefreshToken(1, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyAccessToken(access); err != nil {
		t.Fatalf("access token rejected: %v", err)
	}
	if _, err := manager.VerifyRefreshToken(refresh); err != nil {
		t.Fatalf("refresh token rejected: %v", err)
	}
	if _, err := manager.VerifyAccessToken(refresh); err == nil {
		t.Fatal("refresh token accepted as access token")
	}
	if _, err := manager.VerifyRefreshToken(access); err == nil {
		t.Fatal("access token accepted as refresh token")
	}
}

func TestTokenRequiresIdentityAndLifetimeClaims(t *testing.T) {
	manager := NewJWTManager("test-secret", 1, 1)
	malformed := jwt.NewWithClaims(jwt.SigningMethodHS256, CustomClaims{
		UserID:   1,
		Username: "alice",
		TokenUse: TokenUseAccess,
	})
	signed, err := malformed.SignedString(manager.secretKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyAccessToken(signed); err == nil {
		t.Fatal("token without registered identity and lifetime claims was accepted")
	}
}
