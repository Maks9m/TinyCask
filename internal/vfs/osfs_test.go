//go:build unix

package vfs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/vfstest"
)

func TestOSFS_Conformance(t *testing.T) {
	t.Parallel()
	vfstest.RunFSTests(t, func(testing.TB) vfs.FS { return vfs.OSFS{} })
}

func TestOSFS_CreateUsesFilePermissions(t *testing.T) {
	t.Parallel()
	name := filepath.Join(t.TempDir(), "a")
	f, err := vfs.OSFS{}.Create(name)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	fi, err := os.Stat(name)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
	// The umask may only clear bits, never add them.
	assert.Zero(t, fi.Mode().Perm()&^fs.FileMode(0o644))
}

func TestOSFS_WritesReachDisk(t *testing.T) {
	t.Parallel()
	name := filepath.Join(t.TempDir(), "a")
	f, err := vfs.OSFS{}.Create(name)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello world"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(5))
	_, err = f.Write([]byte("!"))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())

	got, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, "hello!", string(got))
}

func TestOSFS_LockFileStaysAfterRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name := filepath.Join(dir, "LOCK")
	l, err := vfs.OSFS{}.Lock(name)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	// Removing the file on release would let two openers lock different
	// inodes under the same name.
	_, err = os.Stat(name)
	require.NoError(t, err)
}

func TestOSFS_LockOnDirectoryFails(t *testing.T) {
	t.Parallel()
	l, err := vfs.OSFS{}.Lock(t.TempDir())
	require.Error(t, err)
	assert.NotErrorIs(t, err, vfs.ErrLocked)
	assert.Nil(t, l)
}

func TestOSFS_SyncDirMissingIsNotTranslated(t *testing.T) {
	t.Parallel()
	// Only ENOSPC is translated; a missing directory keeps its ErrNotExist.
	err := vfs.OSFS{}.SyncDir(filepath.Join(t.TempDir(), "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.NotErrorIs(t, err, vfs.ErrNoSpace)
}
