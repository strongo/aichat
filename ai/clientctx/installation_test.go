// Copyright 2026 Sneat.app

package clientctx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationIDPersistsAndResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app", "installation_id")
	a, err := InstallationID(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := InstallationID(path)
	if err != nil || b != a {
		t.Fatalf("persisted ID = %q, %v; want %q", b, err, a)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c, err := InstallationID(path)
	if err != nil || c == a {
		t.Fatalf("reset ID = %q, %v; old %q", c, err, a)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNewUUIDReadFailure(t *testing.T) {
	if _, err := newUUIDFrom(failingReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error=%v", err)
	}
}

func TestInstallationIDFilesystemFailures(t *testing.T) {
	failure := errors.New("injected failure")
	base := defaultInstallationOps()
	tests := []struct {
		name  string
		alter func(*installationOps)
	}{
		{"read", func(op *installationOps) { op.read = func(string) (string, error) { return "", failure } }},
		{"mkdir", func(op *installationOps) { op.mkdir = func(string, os.FileMode) error { return failure } }},
		{"uuid", func(op *installationOps) { op.uuid = func() (string, error) { return "", failure } }},
		{"create", func(op *installationOps) { op.create = func(string, string) (*os.File, error) { return nil, failure } }},
		{"chmod", func(op *installationOps) { op.chmod = func(*os.File, os.FileMode) error { return failure } }},
		{"write", func(op *installationOps) { op.write = func(*os.File, string) (int, error) { return 0, failure } }},
		{"close", func(op *installationOps) { op.close = func(f *os.File) error { _ = f.Close(); return failure } }},
		{"link", func(op *installationOps) { op.link = func(string, string) error { return failure } }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := base
			tt.alter(&op)
			path := filepath.Join(t.TempDir(), "app", "installation_id")
			if _, err := installationID(path, op); !errors.Is(err, failure) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("persistent path after failure: %v", err)
			}
		})
	}
}

func TestInstallationIDLinkRaceUsesExistingValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation_id")
	existing, err := InstallationID(path)
	if err != nil {
		t.Fatal(err)
	}
	op := defaultInstallationOps()
	first := true
	op.read = func(p string) (string, error) {
		// Simulate the first read racing a peer that created the file.
		if first {
			first = false
			return "", os.ErrNotExist
		}
		return readID(p)
	}
	// The first read in installationID uses the custom one, while the final
	// read of an already-linked target uses readID after the simulated race.
	got, err := installationID(path, op)
	if err != nil || got != existing {
		t.Fatalf("got=%q err=%v want=%q", got, err, existing)
	}
}

func TestReadIDRejectsInvalidFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation_id")
	for _, content := range []string{"not-a-uuid", "550e8400-e29b-41d4-a716-44665544000z"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readID(path); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
}
