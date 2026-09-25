// Copyright 2026 Sneat.app

// Package clientctx holds reusable, privacy-bounded caller context helpers.
package clientctx

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstallationID returns a random UUID persisted at path. The file is
// atomically linked into place, so concurrent CLI processes keep one ID.
// Deleting the configuration directory intentionally resets the identity.
func InstallationID(path string) (string, error) {
	return installationID(path, defaultInstallationOps())
}

func defaultInstallationOps() installationOps {
	return installationOps{
		read: readID, mkdir: os.MkdirAll, uuid: NewUUID,
		create: os.CreateTemp, chmod: (*os.File).Chmod,
		write: (*os.File).WriteString, close: (*os.File).Close,
		link: os.Link,
	}
}

// installationOps is a narrow test seam for filesystem failure paths. A
// failed ID creation must never leave a partial persistent identifier.
type installationOps struct {
	read   func(string) (string, error)
	mkdir  func(string, os.FileMode) error
	uuid   func() (string, error)
	create func(string, string) (*os.File, error)
	chmod  func(*os.File, os.FileMode) error
	write  func(*os.File, string) (int, error)
	close  func(*os.File) error
	link   func(string, string) error
}

func installationID(path string, op installationOps) (string, error) {
	if id, err := op.read(path); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := op.mkdir(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create installation directory: %w", err)
	}
	id, err := op.uuid()
	if err != nil {
		return "", err
	}
	tmp, err := op.create(filepath.Dir(path), ".installation-id-*")
	if err != nil {
		return "", fmt.Errorf("create installation ID temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := op.chmod(tmp, 0600); err != nil {
		_ = op.close(tmp)
		return "", err
	}
	if _, err := op.write(tmp, id+"\n"); err != nil {
		_ = op.close(tmp)
		return "", err
	}
	if err := op.close(tmp); err != nil {
		return "", err
	}
	if err := op.link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("persist installation ID: %w", err)
	}
	return op.read(path)
}

// NewUUID returns a cryptographically random version-4 UUID suitable for
// client correlation. It carries no hardware or account information.
func NewUUID() (string, error) {
	return newUUIDFrom(rand.Reader)
}

func newUUIDFrom(source io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(source, b[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:])), nil
}

func readID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return "", fmt.Errorf("invalid installation ID in %s", path)
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("invalid installation ID in %s", path)
		}
	}
	return id, nil
}
