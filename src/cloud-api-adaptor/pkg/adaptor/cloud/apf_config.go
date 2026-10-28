// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeAPFConfig replaces the debug copy with a private inode, including when
// the existing path has permissive permissions or refers to a link.
func writeAPFConfig(path string, data []byte) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".apf-*.json")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if cleanupErr := os.Remove(file.Name()); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("removing temporary APF configuration: %w", cleanupErr))
			}
		}
	}()
	if _, err = file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
