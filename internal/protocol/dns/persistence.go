// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/imksoo/routerd/pkg/logstore"
)

func writeAtomicPrivate(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".dns-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Event history is bounded in memory and on disk. Persistence is optional;
// failures are counted in status and sent to the module logger by the caller.
func appendEvent(path string, event any) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("DNS event path is not a regular file")
		}
		if info.Size()+int64(len(data))+1 > 4<<20 {
			if err := os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	return err
}

func openQueryLog(path string) (*logstore.DNSQueryLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("DNS query log path is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return logstore.OpenDNSQueryLog(path)
}
