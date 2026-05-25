package hub

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net"

	gossh "golang.org/x/crypto/ssh"
)

func (s *Server) sshHostKeyCallback(host string) gossh.HostKeyCallback {
	return func(_ string, _ net.Addr, key gossh.PublicKey) error {
		return s.verifySSHHostKey(host, key)
	}
}

func (s *Server) verifySSHHostKey(host string, key gossh.PublicKey) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ssh host key store is not available")
	}
	keyType := key.Type()
	keyData := base64.StdEncoding.EncodeToString(key.Marshal())
	fingerprint := gossh.FingerprintSHA256(key)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ssh host key transaction: %w", err)
	}
	defer tx.Rollback()

	var storedType, storedKey, storedFingerprint string
	err = tx.QueryRow(`SELECT key_type, key_data, fingerprint FROM ssh_known_hosts WHERE host=?`, host).Scan(&storedType, &storedKey, &storedFingerprint)
	if err == sql.ErrNoRows {
		_, err = tx.Exec(`
			INSERT INTO ssh_known_hosts(host, key_type, key_data, fingerprint, first_seen_at, last_seen_at)
			VALUES(?,?,?,?,?,?)
		`, host, keyType, keyData, fingerprint, now(), now())
		if err != nil {
			return fmt.Errorf("store ssh host key for %s: %w", host, err)
		}
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("load ssh host key for %s: %w", host, err)
	}
	if storedType != keyType || storedKey != keyData {
		return fmt.Errorf("ssh host key changed for %s: expected %s %s, got %s %s", host, storedType, storedFingerprint, keyType, fingerprint)
	}
	if _, err := tx.Exec(`UPDATE ssh_known_hosts SET last_seen_at=? WHERE host=?`, now(), host); err != nil {
		return fmt.Errorf("update ssh host key for %s: %w", host, err)
	}
	return tx.Commit()
}
