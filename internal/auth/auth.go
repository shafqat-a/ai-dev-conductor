package auth

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

type AuthService struct {
	passwordHash []byte
}

func NewAuthService(password string) (*AuthService, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &AuthService{passwordHash: hash}, nil
}

func (a *AuthService) VerifyPassword(password string) bool {
	return bcrypt.CompareHashAndPassword(a.passwordHash, []byte(password)) == nil
}

func GenerateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
