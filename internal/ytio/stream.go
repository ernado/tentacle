package ytio

import (
	"context"
	"os"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
)

type StreamPart struct {
	Offset int64
	Last   bool
	Data   []byte
	Index  int
}

func StreamFile(
	ctx context.Context,
	done chan struct{},
	filePath string,
	partSize int64,
	fn func(part StreamPart) error,
) error {
	lg := zctx.From(ctx)
	f, err := os.OpenFile(filePath, os.O_RDONLY, 0o644)
	if err != nil {
		return errors.Wrap(err, "open file")
	}
	var (
		lastPartAvailable bool
		offset            int64
		index             int
	)
	go func() {
		<-done
		lastPartAvailable = true
		lg.Info("Last part available")
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Polling.
			// Check with stat to see if file size changed.
			info, err := f.Stat()
			if err != nil {
				return errors.Wrap(err, "stat file")
			}
			// offset + partSize is the end of the next part.
			startOfNextPart := offset
			if _, err = f.Seek(startOfNextPart, 0); err != nil {
				return errors.Wrap(err, "seek file")
			}
			tailSize := info.Size() - startOfNextPart
			if tailSize > partSize {
				// Enough data for the next part.
				data := make([]byte, partSize)
				if _, err := f.Read(data); err != nil {
					return errors.Wrap(err, "read part")
				}
				lg.Info("Uploading part")
				if err := fn(StreamPart{
					Offset: startOfNextPart,
					Last:   false,
					Data:   data,
					Index:  index,
				}); err != nil {
					return errors.Wrap(err, "send part")
				}
				offset += partSize
				index++
				continue
			}
			if lastPartAvailable {
				// Read last part.
				lg.Info("Uploading last part")
				data := make([]byte, tailSize)
				if _, err := f.Read(data); err != nil {
					return errors.Wrap(err, "read last part")
				}
				if err := fn(StreamPart{
					Offset: startOfNextPart,
					Last:   true,
					Data:   data,
					Index:  index,
				}); err != nil {
					return errors.Wrap(err, "send last part")
				}
				return nil
			} else {
				// Not enough data yet.
				lg.Info("Not enough data yet")
				continue
			}
		}
	}
}
