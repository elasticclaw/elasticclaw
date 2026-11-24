package hub

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// errReplicatedVMUnassigned means the claw no longer owns the VM (deleted or
// replaced), so the caller should stop quietly rather than fail the claw.
var errReplicatedVMUnassigned = errors.New("replicated VM is no longer assigned to the claw")

func (s *Server) assignReplicatedVM(clawID, vmID string) error {
	_, err := s.db.Exec(`
		UPDATE claws SET provider='replicated', provider_id=?,
			ssh_host=CASE WHEN provider_id != ? THEN '' ELSE ssh_host END,
			ssh_port=CASE WHEN provider_id != ? THEN 0 ELSE ssh_port END,
			ssh_user=CASE WHEN provider_id != ? THEN '' ELSE ssh_user END
		WHERE id=? AND status NOT IN ('deleted','starting','connected','idle')
	`, vmID, vmID, vmID, vmID, clawID)
	return err
}

func (s *Server) storeReplicatedSSHEndpoint(clawID, vmID, host string, port int, user string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = s.storeReplicatedSSHEndpointOnce(clawID, vmID, host, port, user)
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

func (s *Server) storeReplicatedSSHEndpointOnce(clawID, vmID, host string, port int, user string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// An empty endpoint marks a newly assigned VM. Clear its old pin only once,
	// atomically with storing the endpoint, so bootstrap retries retain trust.
	addr := fmt.Sprintf("%s:%d", host, port)
	result, err := tx.Exec(`
		DELETE FROM ssh_known_hosts WHERE host=? AND EXISTS (
			SELECT 1 FROM claws WHERE id=? AND provider='replicated' AND provider_id=? AND ssh_host=''
		)
	`, addr, clawID, vmID)
	if err != nil {
		return err
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return err
	}
	result, err = tx.Exec(`
		UPDATE claws SET ssh_host=?, ssh_port=?, ssh_user=?
		WHERE id=? AND provider='replicated' AND provider_id=?
	`, host, port, user, clawID, vmID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("%w: vm %s, claw %s", errReplicatedVMUnassigned, vmID, clawID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if cleared > 0 {
		log.Printf("[bootstrap] cleared stale SSH host key for %s for new replicated VM %s", addr, vmID)
	}
	return nil
}

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
