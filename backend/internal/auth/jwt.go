package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	minSecretBytes = 32
	bcryptCost     = bcrypt.DefaultCost
)

var (
	ErrInvalidToken = errors.New("认证令牌无效或格式错误")
	ErrExpiredToken = errors.New("认证令牌已过期")
)

// Claims 描述管理员 JWT 令牌中包含的核心声明。
type Claims struct {
	AdminID  int64  `json:"sub"`
	Username string `json:"username"`
	Role     string `json:"role"`
	PwdFP    string `json:"pfp,omitempty"`
	IssuedAt int64  `json:"iat"`
	ExpireAt int64  `json:"exp"`
}

// PasswordFingerprint 计算密码哈希前缀指纹，用于在改密后使旧令牌立即失效。
func PasswordFingerprint(passwordHash string) string {
	if passwordHash == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(passwordHash))
	return fmt.Sprintf("%x", sum[:8])
}

// jwtHeader 描述固定使用的 HS256 JWT 头部。
type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

// HashPassword 使用 bcrypt 生成管理员密码哈希。
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("管理员密码长度不能少于 8 位")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希失败：%w", err)
	}
	return string(hashed), nil
}

// VerifyPassword 校验明文密码是否与持久化 bcrypt 哈希匹配。
func VerifyPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// GenerateRandomSecret 生成至少 32 字节的随机 HMAC 密钥。
func GenerateRandomSecret() ([]byte, error) {
	secret := make([]byte, minSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("生成随机 JWT 密钥失败：%w", err)
	}
	return secret, nil
}

// SignToken 使用 HS256 对管理员 Claims 签名并输出 JWT 字符串。
func SignToken(secret []byte, claims Claims) (string, error) {
	if len(secret) < minSecretBytes {
		return "", errors.New("JWT 签名密钥长度不足 32 字节")
	}
	if claims.AdminID <= 0 || strings.TrimSpace(claims.Username) == "" || strings.TrimSpace(claims.Role) == "" {
		return "", errors.New("JWT 声明缺少必要的管理员字段")
	}
	headerBytes, err := json.Marshal(jwtHeader{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("编码 JWT 头部失败：%w", err)
	}
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("编码 JWT 载荷失败：%w", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := signHS256(secret, unsigned)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyToken 验证 HS256 JWT 签名与有效期，并返回解析后的 Claims。
func VerifyToken(secret []byte, token string, now time.Time) (Claims, error) {
	if len(secret) < minSecretBytes {
		return Claims{}, errors.New("JWT 验证密钥长度不足 32 字节")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return Claims{}, ErrInvalidToken
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	expected := signHS256(secret, parts[0]+"."+parts[1])
	if !hmac.Equal(sigBytes, expected) {
		return Claims{}, ErrInvalidToken
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if claims.AdminID <= 0 || strings.TrimSpace(claims.Username) == "" || claims.ExpireAt <= 0 {
		return Claims{}, ErrInvalidToken
	}
	if now.Unix() >= claims.ExpireAt {
		return Claims{}, ErrExpiredToken
	}
	return claims, nil
}

// signHS256 计算 JWT 签名段落的 HMAC-SHA256 摘要。
func signHS256(secret []byte, unsigned string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	return mac.Sum(nil)
}
