package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPasswordHashAndVerify 验证 bcrypt 密码哈希生成与校验行为。
func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("Admin@2026")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !VerifyPassword(hash, "Admin@2026") {
		t.Fatal("VerifyPassword() = false, want true")
	}
	if VerifyPassword(hash, "WrongPassword") {
		t.Fatal("VerifyPassword() = true for wrong password")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error for short password")
	}
}

// TestJWTSignAndVerify 验证 HS256 JWT 签发、解析、篡改拒绝与过期校验。
func TestJWTSignAndVerify(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	token, err := SignToken(secret, Claims{
		AdminID:  1,
		Username: "admin",
		Role:     "ADMIN",
		IssuedAt: now.Unix(),
		ExpireAt: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken() error = %v", err)
	}

	claims, err := VerifyToken(secret, token, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("VerifyToken() error = %v", err)
	}
	if claims.AdminID != 1 || claims.Username != "admin" || claims.Role != "ADMIN" {
		t.Fatalf("claims = %+v", claims)
	}

	if _, err := VerifyToken(secret, token+"tamper", now.Add(10*time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token err = %v, want ErrInvalidToken", err)
	}

	parts := strings.Split(token, ".")
	parts[1] = parts[1] + "a"
	if _, err := VerifyToken(secret, strings.Join(parts, "."), now.Add(10*time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("modified payload err = %v, want ErrInvalidToken", err)
	}

	if _, err := VerifyToken(secret, token, now.Add(2*time.Hour)); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expired token err = %v, want ErrExpiredToken", err)
	}
}
