package ytio

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func calculateHash(t *testing.T, filePath string) string {
	f, err := os.Open(filePath)
	require.NoError(t, err)

	h := sha256.New()
	require.NoError(t, err)

	_, err = io.Copy(h, f)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestFile(t *testing.T) {
	dir := t.TempDir()
	filePath := dir + "/test.bin"

	// Fill file with some random data.
	source := rand.NewSource(1)
	rnd := rand.New(source)

	f, err := os.Create(filePath)
	require.NoError(t, err)

	size := int64(11*1024*1024) + 356 // 10 MiB + 356 bytes

	_, err = io.CopyN(f, rnd, size)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	originalHash := calculateHash(t, filePath)
	t.Logf("Original hash: %s", originalHash)

	file := &File{
		Path: filePath + ".allocated",
		Size: size,
	}

	require.NoError(t, file.Allocate())

	partSize := int64(1024*1024 + 101)
	file.Split(partSize)

	t.Logf("Parts len: %d", len(file.Parts))

	g, _ := errgroup.WithContext(t.Context())
	for _, part := range file.Parts {
		g.Go(func() error {
			t.Logf("Copying part: offset=%d size=%d", part.Offset, part.Size)

			src, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()

			if _, err := src.Seek(part.Offset, 0); err != nil {
				return err
			}

			dst, err := os.OpenFile(part.FilePath, os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer func() { _ = dst.Close() }()

			if _, err := dst.Seek(part.Offset, 0); err != nil {
				return err
			}

			_, err = io.CopyN(dst, src, part.Size)
			return err
		})
	}
	require.NoError(t, g.Wait())

	finalHash := calculateHash(t, file.Path)
	t.Logf("Final hash: %s", finalHash)
}
