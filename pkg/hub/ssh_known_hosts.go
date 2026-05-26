package hub

import (
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func (s *Server) sshHostKeyCallback(host string) gossh.HostKeyCallback {
	return func(_ string, _ net.Addr, key gossh.PublicKey) error {
		return s.verifySSHHostKey(host, key)
	}
}

func (s *Server) verifySSHHostKey(host string, key gossh.PublicKey) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = s.verifySSHHostKeyOnce(host, key)
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

func (s *Server) verifySSHHostKeyOnce(host string, key gossh.PublicKey) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ssh host key store is not available")
	}
	keyType := key.Type()
	keyData := base64.StdEncoding.EncodeToString(key.Marshal())
	fingerprint := gossh.FingerprintSHA256(key)

	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO ssh_known_hosts(host, key_type, key_data, fingerprint, first_seen_at, last_seen_at)
		VALUES(?,?,?,?,?,?)
	`, host, keyType, keyData, fingerprint, now(), now()); err != nil {
		return fmt.Errorf("store ssh host key for %s: %w", host, err)
	}

	var storedType, storedKey, storedFingerprint string
	if err := s.db.QueryRow(`SELECT key_type, key_data, fingerprint FROM ssh_known_hosts WHERE host=?`, host).Scan(&storedType, &storedKey, &storedFingerprint); err != nil {
		return fmt.Errorf("load ssh host key for %s: %w", host, err)
	}
	if storedType != keyType || storedKey != keyData {
		return fmt.Errorf("ssh host key changed for %s: expected %s %s, got %s %s", host, storedType, storedFingerprint, keyType, fingerprint)
	}
	if _, err := s.db.Exec(`UPDATE ssh_known_hosts SET last_seen_at=? WHERE host=?`, now(), host); err != nil {
		return fmt.Errorf("update ssh host key for %s: %w", host, err)
	}
	return nil
}

func isSQLiteBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database is locked")
}
