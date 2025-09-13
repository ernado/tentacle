package ytio

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-faster/errors"
	httpio "github.com/gotd/contrib/http_io"
)

type Part struct {
	Offset    int64
	Size      int64
	FilePath  string
	Available bool
	Mux       sync.Mutex
}

func (p *Part) SetAvailable() {
	p.Mux.Lock()
	defer p.Mux.Unlock()
	p.Available = true
}

func (p *Part) IsAvailable() bool {
	p.Mux.Lock()
	defer p.Mux.Unlock()
	return p.Available
}

// File is partially downloaded file.
type File struct {
	Path  string
	Size  int64
	Parts []*Part
}

func (f *File) PartAt(offset int64) *Part {
	for _, p := range f.Parts {
		if p.Offset == offset {
			return p
		}
	}
	return nil
}

func (f *File) StreamAt(ctx context.Context, skip int64, w io.Writer) error {
	file, err := os.OpenFile(f.Path, os.O_RDONLY, 0o644)
	if err != nil {
		return errors.Wrap(err, "open file")
	}
	defer func() {
		_ = file.Close()
	}()
	if _, err := file.Seek(skip, 0); err != nil {
		return errors.Wrap(err, "seek file")
	}

	// Wait until first part is available.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if f.PartAt(skip).IsAvailable() {
				// Write as much as possible until next part.
				var toWrite int64
				for _, p := range f.Parts {
					if p.Offset < skip {
						continue
					}
					if !p.IsAvailable() {
						break
					}
					toWrite += p.Size
				}
				if toWrite == 0 {
					continue
				}
				n, err := io.CopyN(w, file, toWrite)
				if err != nil {
					return errors.Wrap(err, "copy file")
				}
				skip += n
				if skip >= f.Size {
					return nil
				}
			}
		}
	}
}

func (f *File) Split(partSize int64) {
	if partSize <= 0 {
		partSize = 1024 * 1024 // 1 MiB
	}
	f.Parts = make([]*Part, 0)
	var offset int64
	for offset < f.Size {
		size := partSize
		if offset+size > f.Size {
			size = f.Size - offset
		}
		f.Parts = append(f.Parts, &Part{
			FilePath: f.Path,
			Offset:   offset,
			Size:     size,
		})
		offset += size
	}
}

func (f *File) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Parse
	httpio.NewHandler(f, f.Size).ServeHTTP(w, r)
}

// Allocate creates file on disk with specified size.
func (f *File) Allocate() error {
	file, err := os.Create(f.Path)
	if err != nil {
		return errors.Wrap(err, "create file")
	}
	defer func() {
		_ = file.Close()
	}()
	if err := file.Truncate(f.Size); err != nil {
		return errors.Wrap(err, "truncate file")
	}
	if err := file.Close(); err != nil {
		return errors.Wrap(err, "close file")
	}
	return nil
}
