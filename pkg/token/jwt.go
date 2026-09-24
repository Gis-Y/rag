// Package token 提供了用于生成和验证 JSON Web Tokens (JWT) 的功能。
package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	TokenUseAccess  = "access"
	TokenUseRefresh = "refresh"
)

// JWTManager 负责管理 JWT 的生成和验证。
type JWTManager struct {
	secretKey       []byte        // secretKey 用于签名和验证 token 的密钥
	accessTokenDur  time.Duration // accessTokenDur 定义了 access token 的有效期
	refreshTokenDur time.Duration // refreshTokenDur 定义了 refresh token 的有效期
}

// CustomClaims 定义了我们想要在 JWT 中存储的自定义数据。
// 它嵌入了 jwt.RegisteredClaims 以包含标准的 JWT 声明（如过期时间）。
type CustomClaims struct {
	UserID   uint   `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	TokenUse string `json:"tokenUse"`
	jwt.RegisteredClaims
}

// NewJWTManager 创建一个新的 JWTManager 实例。
// secret: 用于签名的密钥字符串。
// accessTokenExpireHours: access token 的过期时间（小时）。
// refreshTokenExpireDays: refresh token 的过期时间（天）。
func NewJWTManager(secret string, accessTokenExpireHours, refreshTokenExpireDays int) *JWTManager {
	return &JWTManager{
		secretKey:       []byte(secret),
		accessTokenDur:  time.Hour * time.Duration(accessTokenExpireHours),
		refreshTokenDur: time.Duration(refreshTokenExpireDays) * 24 * time.Hour,
	}
}

// GenerateToken 根据给定的用户信息生成一个新的 access token。
func (m *JWTManager) GenerateToken(userID uint, username, role string) (string, error) {
	return m.generateToken(userID, username, role, TokenUseAccess, m.accessTokenDur)
}

// GenerateRefreshToken 根据给定的用户信息生成一个新的 refresh token。
func (m *JWTManager) GenerateRefreshToken(userID uint, username, role string) (string, error) {
	return m.generateToken(userID, username, role, TokenUseRefresh, m.refreshTokenDur)
}

func (m *JWTManager) generateToken(userID uint, username, role, tokenUse string, duration time.Duration) (string, error) {
	now := time.Now()
	id, err := GenerateRandomString(16)
	if err != nil {
		return "", err
	}
	claims := CustomClaims{
		UserID:   userID,
		Username: username,
		Role:     role,
		TokenUse: tokenUse,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        id,
			ExpiresAt: jwt.NewNumericDate(now.Add(duration)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secretKey)
}

func (m *JWTManager) VerifyAccessToken(tokenString string) (*CustomClaims, error) {
	return m.verifyToken(tokenString, TokenUseAccess)
}

func (m *JWTManager) VerifyRefreshToken(tokenString string) (*CustomClaims, error) {
	return m.verifyToken(tokenString, TokenUseRefresh)
}

func (m *JWTManager) verifyToken(tokenString, expectedUse string) (*CustomClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &CustomClaims{}, func(token *jwt.Token) (interface{}, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return m.secretKey, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*CustomClaims); ok &&
		token.Valid &&
		claims.TokenUse == expectedUse &&
		claims.UserID != 0 &&
		claims.Username != "" &&
		claims.ID != "" &&
		claims.IssuedAt != nil &&
		claims.ExpiresAt != nil {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

// GenerateRandomString generates a random hex string of a given length.
func GenerateRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
