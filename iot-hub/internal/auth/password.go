package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
)

// People sign in with an email address and a password; devices and services
// keep tokens and SAS keys, which is all they can present.
//
// Passwords are stored as PBKDF2-HMAC-SHA256 with a per-user random salt.
// PBKDF2 is in the standard library (Go 1.24), so this costs no dependency;
// the iteration count is the OWASP 2023 figure for that construction.
const (
	pbkdfIter   = 600_000
	pbkdfLen    = 32
	saltLen     = 16
	MinPassword = 10
	MaxPassword = 1024 // a long passphrase is fine; a megabyte is an attack
	MaxEmailLen = 254
)

// Password is a stored password verifier. The password itself is never kept.
type Password struct {
	Salt string `json:"salt"` // base64
	Hash string `json:"hash"` // base64
	Iter int    `json:"iter"`
}

// NewPassword hashes pw for storage.
func NewPassword(pw string) (*Password, error) {
	if err := CheckPassword(pw); err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	sum, err := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, pbkdfLen)
	if err != nil {
		return nil, err
	}
	return &Password{Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(sum), Iter: pbkdfIter}, nil
}

// verify reports whether pw matches, in constant time.
func (p *Password) verify(pw string) bool {
	if p == nil || p.Iter <= 0 || len(pw) > MaxPassword {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(p.Salt)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(p.Hash)
	if err != nil {
		return false
	}
	sum, err := pbkdf2.Key(sha256.New, pw, salt, p.Iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(sum, want) == 1
}

// CheckPassword rejects passwords that are too short or too long. Length is
// the only rule: composition rules push people towards "Passw0rd!".
func CheckPassword(pw string) error {
	if len(pw) < MinPassword {
		return fmt.Errorf("%w: password must be at least %d characters", ErrInvalid, MinPassword)
	}
	if len(pw) > MaxPassword {
		return fmt.Errorf("%w: password must be at most %d characters", ErrInvalid, MaxPassword)
	}
	return nil
}

// NormalEmail lowercases and trims an address, and checks it parses. The
// normalised form is what is stored and compared, so Ana@x.co and ana@x.co
// are one account.
func NormalEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return "", fmt.Errorf("%w: email is required", ErrInvalid)
	}
	if len(e) > MaxEmailLen {
		return "", fmt.Errorf("%w: email is too long", ErrInvalid)
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e, "@") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, email)
	}
	return e, nil
}

// CanSignIn reports whether a role is a person, and so may have a password.
func CanSignIn(r Role) bool { return r == Admin || r == Operator || r == Viewer }
