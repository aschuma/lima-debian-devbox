package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxKeySize is generous for any OpenSSH key, including certificates.
const maxKeySize = 64 << 10

var keyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SSHKey names a host private key to install into the guest's ~/.ssh.
type SSHKey struct {
	Source string `toml:"source" json:"source"`
	Name   string `toml:"name" json:"name"`
}

// keyFile is a key read from the host, ready for upload.
type keyFile struct {
	Name    string
	Private []byte
	Public  []byte // nil when no .pub exists next to the private key
}

func (c *Config) validateSSHKeys() error {
	seen := make(map[string]bool, len(c.SSHKeys))
	for i := range c.SSHKeys {
		k := &c.SSHKeys[i]
		if k.Source == "" {
			return fmt.Errorf("ssh_keys[%d]: source is required", i+1)
		}
		if strings.HasPrefix(k.Source, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			k.Source = filepath.Join(home, k.Source[2:])
		} else if !filepath.IsAbs(k.Source) {
			k.Source = filepath.Join(c.Root, k.Source)
		}
		if k.Name == "" {
			k.Name = filepath.Base(k.Source)
		}
		if !keyName.MatchString(k.Name) || strings.HasSuffix(k.Name, ".pub") {
			return fmt.Errorf("ssh_keys[%d]: name must be a plain file name without .pub", i+1)
		}
		if seen[k.Name] {
			return fmt.Errorf("ssh_keys[%d]: duplicate name %q", i+1, k.Name)
		}
		seen[k.Name] = true
	}
	return nil
}

// loadSSHKeys reads the configured keys from the host. Contents never reach
// error messages.
func loadSSHKeys(keys []SSHKey) ([]keyFile, error) {
	files := make([]keyFile, 0, len(keys))
	for _, k := range keys {
		private, err := readKey(k.Source)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w", k.Name, err)
		}
		public, err := readKey(k.Source + ".pub")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ssh key %s.pub: %w", k.Name, err)
		}
		files = append(files, keyFile{Name: k.Name, Private: private, Public: public})
	}
	return files, nil
}

func readKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() == 0 || info.Size() > maxKeySize {
		return nil, fmt.Errorf("size must be 1..%d bytes", maxKeySize)
	}
	return os.ReadFile(path)
}
