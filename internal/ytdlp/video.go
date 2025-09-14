package ytdlp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/dustin/go-humanize"
	"github.com/ernado/tentacle/internal/ytio"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Fragment struct {
	URL      string  `json:"url"`
	Duration float64 `json:"duration"`
}

type DownloaderOptions struct {
	HTTPChunkSize int64 `json:"http_chunk_size"`
}

type Format struct {
	FormatID          string            `json:"format_id"`
	FormatNote        string            `json:"format_note"`
	Ext               string            `json:"ext"`
	Protocol          string            `json:"protocol"`
	ACodec            string            `json:"acodec"`
	VCodec            string            `json:"vcodec"`
	URL               string            `json:"url"`
	Width             int               `json:"width"`
	Height            int               `json:"height"`
	FPS               float64           `json:"fps"`
	Rows              int               `json:"rows"`
	Columns           int               `json:"columns"`
	Fragments         []Fragment        `json:"fragments"`
	AudioExt          string            `json:"audio_ext"`
	VideoExt          string            `json:"video_ext"`
	VBR               float64           `json:"vbr"`
	ABR               float64           `json:"abr"`
	TBR               float64           `json:"tbr"`
	Resolution        string            `json:"resolution"`
	AspectRatio       float64           `json:"aspect_ratio"`
	FilesizeApprox    int64             `json:"filesize_approx"`
	HTTPHeaders       map[string]string `json:"http_headers"`
	Format            string            `json:"format"`
	DownloaderOptions DownloaderOptions `json:"downloader_options"`
}

type Video struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Formats []Format `json:"formats"`
}

func BestVideo(formats []Format) Format {
	var best Format
	for _, f := range formats {
		if f.VCodec == "none" {
			continue
		}
		best = f
	}

	return best
}

func BestAudio(formats []Format) Format {
	var best Format
	for _, f := range formats {
		if f.ACodec == "none" {
			continue
		}
		best = f
	}

	return best
}

func SelectFormats(formats []Format) []Format {
	var selected []Format

	// Video candidates.
	for _, format := range formats {
		// Format id=398 ext=mp4 acodec=none vcodec=av01.0.05M.08 resolution=720x1280
		if format.Ext != "mp4" || format.VCodec == "none" {
			continue
		}
		selected = append(selected, format)
	}

	// Audio candidates.
	for _, format := range formats {
		// Format id=140 ext=m4a acodec=mp4a.40.2 vcodec=none resolution="audio only"
		if format.VCodec != "none" || format.ACodec == "none" || format.Ext != "m4a" {
			continue
		}
		selected = append(selected, format)
	}

	return selected
}

func NewHTTPClientWithProxy(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return http.DefaultClient, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(u),
	}
	return &http.Client{Transport: transport}, nil
}

func FormatExactSize(ctx context.Context, format Format, httpClient *http.Client) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", format.URL, nil)
	if err != nil {
		return 0, errors.Wrap(err, "create request")
	}
	for k, v := range format.HTTPHeaders {
		req.Header.Set(k, v)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, errors.Wrap(err, "do request")
	}
	defer func() {
		_ = res.Body.Close()
	}()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return 0, errors.Errorf("bad status: %s: %q", res.Status, body)
	}
	return res.ContentLength, nil
}

func DownloadPart(ctx context.Context, format Format, part *ytio.Part, httpClient *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", format.URL, nil)
	if err != nil {
		return errors.Wrap(err, "create request")
	}
	for k, v := range format.HTTPHeaders {
		req.Header.Set(k, v)
	}

	// Only set Range header if it's not the entire file
	if part.Offset > 0 || part.Size < format.FilesizeApprox {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", part.Offset, part.Offset+part.Size-1))
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "do request")
	}
	defer func() {
		_ = res.Body.Close()
	}()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return errors.Errorf("bad status: %s: %q", res.Status, body)
	}

	f, err := os.OpenFile(part.FilePath, os.O_WRONLY, 0o644)
	if err != nil {
		return errors.Wrap(err, "open part file")
	}
	defer func() {
		_ = f.Close()
	}()
	if _, err := f.Seek(part.Offset, 0); err != nil {
		return errors.Wrap(err, "seek part file")
	}

	written, err := io.Copy(f, res.Body)
	if err != nil {
		return errors.Wrap(err, "copy part data")
	}

	duration := time.Since(start)
	zctx.From(ctx).Info("Downloaded part",
		zap.Int64("offset", part.Offset),
		zap.Int64("expected_size", part.Size),
		zap.Int64("actual_size", written),
		zap.Duration("duration", duration),
		zap.String("speed", humanize.Bytes(uint64(float64(written)/duration.Seconds()))+"/s"),
	)

	part.SetAvailable()

	return nil
}

func DownloadChunked(ctx context.Context, format Format, file *ytio.File, httpClient *http.Client) error {
	exactSize, err := FormatExactSize(ctx, format, httpClient)
	if err != nil {
		return errors.Wrap(err, "get exact size")
	}

	file.Size = exactSize
	if err := file.Allocate(); err != nil {
		return errors.Wrap(err, "allocate video file")
	}

	file.Split(format.DownloaderOptions.HTTPChunkSize)

	parts := make(chan *ytio.Part, len(file.Parts))
	for _, part := range file.Parts {
		parts <- part
	}
	close(parts)

	g, gCtx := errgroup.WithContext(ctx)
	const concurrency = 4
	for i := 0; i < concurrency; i++ {
		g.Go(func() error {
			for part := range parts {
				bo := backoff.NewConstantBackOff(time.Second)
				if err := backoff.Retry(func() error {
					if err := DownloadPart(gCtx, format, part, httpClient); err != nil {
						zctx.From(ctx).Error("Failed to download part", zap.Error(err))
						return errors.Wrap(err, "download part")
					}
					return nil
				}, backoff.WithMaxRetries(bo, 10)); err != nil {
					return errors.Wrap(err, "download part with retry")
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "download parts")
	}

	return nil
}
