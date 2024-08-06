//go:build linux && cgo
// +build linux,cgo

package overlay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containers/storage/pkg/chunked/dump"
	"github.com/containers/storage/pkg/fsverity"
	"github.com/containers/storage/pkg/loopback"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var (
	composeFsHelperOnce sync.Once
	composeFsHelperPath string
	composeFsHelperErr  error
)

func getComposeFsHelper() (string, error) {
	composeFsHelperOnce.Do(func() {
		composeFsHelperPath, composeFsHelperErr = exec.LookPath("mkcomposefs")
	})
	return composeFsHelperPath, composeFsHelperErr
}

func getComposefsBlob(dataDir string) string {
	return filepath.Join(dataDir, "composefs.blob")
}

func generateComposeFsBlob(verityDigests map[string]string, toc interface{}, composefsDir string) error {
	if err := os.MkdirAll(composefsDir, 0o700); err != nil {
		return err
	}

	dumpReader, err := dump.GenerateDump(toc, verityDigests)
	if err != nil {
		return err
	}

	destFile := getComposefsBlob(composefsDir)
	writerJson, err := getComposeFsHelper()
	if err != nil {
		return fmt.Errorf("failed to find mkcomposefs: %w", err)
	}

	outFile, err := os.OpenFile(destFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}

	roFile, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", outFile.Fd()))
	if err != nil {
		outFile.Close()
		return fmt.Errorf("failed to reopen %s as read-only: %w", destFile, err)
	}

	err = func() error {
		// a scope to close outFile before setting fsverity on the read-only fd.
		defer outFile.Close()

		errBuf := &bytes.Buffer{}
		cmd := exec.Command(writerJson, "--from-file", "-", "-")
		cmd.Stderr = errBuf
		cmd.Stdin = dumpReader
		cmd.Stdout = outFile
		if err := cmd.Run(); err != nil {
			rErr := fmt.Errorf("failed to convert json to erofs: %w", err)
			exitErr := &exec.ExitError{}
			if errors.As(err, &exitErr) {
				return fmt.Errorf("%w: %s", rErr, strings.TrimSpace(errBuf.String()))
			}
			return rErr
		}
		return nil
	}()
	if err != nil {
		return err
	}

	if err := fsverity.EnableVerity("manifest file", int(roFile.Fd())); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.ENOTTY) {
		logrus.Warningf("%s", err)
	}

	return nil
}

/*
typedef enum {
	LCFS_EROFS_FLAGS_HAS_ACL = (1 << 0),
} lcfs_erofs_flag_t;

struct lcfs_erofs_header_s {
	uint32_t magic;
	uint32_t version;
	uint32_t flags;
	uint32_t unused[5];
} __attribute__((__packed__));
*/

// hasACL returns true if the erofs blob has ACLs enabled
func hasACL(path string) (bool, error) {
	const (
		LCFS_EROFS_FLAGS_HAS_ACL = (1 << 0)
		versionNumberSize        = 4
		magicNumberSize          = 4
		flagsSize                = 4
	)

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	// do not worry about checking the magic number, if the file is invalid
	// we will fail to mount it anyway
	buffer := make([]byte, versionNumberSize+magicNumberSize+flagsSize)
	nread, err := file.Read(buffer)
	if err != nil {
		return false, err
	}
	if nread != len(buffer) {
		return false, fmt.Errorf("failed to read flags from %q", path)
	}
	flags := buffer[versionNumberSize+magicNumberSize:]
	return binary.LittleEndian.Uint32(flags)&LCFS_EROFS_FLAGS_HAS_ACL != 0, nil
}

func mountComposefsBlob(dataDir, mountPoint string) error {
	blobFile := getComposefsBlob(dataDir)
	loop, err := loopback.AttachLoopDeviceRO(blobFile)
	if err != nil {
		return err
	}
	defer loop.Close()

	hasACL, err := hasACL(blobFile)
	if err != nil {
		return err
	}

	fsfd, err := unix.Fsopen("erofs", 0)
	if err != nil {
		return fmt.Errorf("failed to open erofs filesystem: %w", err)
	}
	defer unix.Close(fsfd)

	if err := unix.FsconfigSetString(fsfd, "source", loop.Name()); err != nil {
		return fmt.Errorf("failed to set source for erofs filesystem: %w", err)
	}

	if err := unix.FsconfigSetFlag(fsfd, "ro"); err != nil {
		return fmt.Errorf("failed to set erofs filesystem read-only: %w", err)
	}

	if !hasACL {
		if err := unix.FsconfigSetFlag(fsfd, "noacl"); err != nil {
			return fmt.Errorf("failed to set noacl for erofs filesystem: %w", err)
		}
	}

	if err := unix.FsconfigCreate(fsfd); err != nil {
		return fmt.Errorf("failed to create erofs filesystem: %w", err)
	}

	mfd, err := unix.Fsmount(fsfd, 0, unix.MOUNT_ATTR_RDONLY)
	if err != nil {
		return fmt.Errorf("failed to mount erofs filesystem: %w", err)
	}
	defer unix.Close(mfd)

	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, mountPoint, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("failed to move mount: %w", err)
	}
	return nil
}
