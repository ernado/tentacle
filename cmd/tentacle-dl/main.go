package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/ernado/tentacle/internal/ytio"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func BestVideo(formats []ytdlp.Format) ytdlp.Format {
	var best ytdlp.Format
	for _, f := range formats {
		if f.VCodec == "none" {
			continue
		}
		best = f
	}

	return best
}

func BestAudio(formats []ytdlp.Format) ytdlp.Format {
	var best ytdlp.Format
	for _, f := range formats {
		if f.ACodec == "none" {
			continue
		}
		best = f
	}

	return best
}

func SelectFormats(formats []ytdlp.Format) []ytdlp.Format {
	var selected []ytdlp.Format

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

func formatExactSize(ctx context.Context, format ytdlp.Format, httpClient *http.Client) (int64, error) {
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

func downloadPart(ctx context.Context, format ytdlp.Format, part *ytio.Part, httpClient *http.Client) error {
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

	// Use io.Copy instead of io.CopyN to handle cases where content length differs
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

func downloadChunked(ctx context.Context, format ytdlp.Format, file *ytio.File, httpClient *http.Client) error {
	exactSize, err := formatExactSize(ctx, format, httpClient)
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
				if err := downloadPart(gCtx, format, part, httpClient); err != nil {
					return errors.Wrap(err, "download part")
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

func run(ctx context.Context, logger *zap.Logger) error {
	var arg struct {
		Cookies  string
		URL      string
		ProxyURL string
	}
	flag.StringVar(&arg.Cookies, "cookies", "cookies.txt", "Path to cookies.txt file")
	flag.StringVar(&arg.URL, "url", "https://www.youtube.com/watch?v=JFLNFJT59DY", "Video URL")
	flag.StringVar(&arg.ProxyURL, "proxy", "", "Proxy URL")
	flag.Parse()

	httpClient, err := NewHTTPClientWithProxy(arg.ProxyURL)
	if err != nil {
		return errors.Wrap(err, "create http client")
	}

	instance := &ytdlp.Instance{
		CookiesFilePath: arg.Cookies,
		Proxy:           arg.ProxyURL,
	}
	start := time.Now()

	video, err := instance.Video(ctx, arg.URL)
	if err != nil {
		return errors.Wrap(err, "fetch video info")
	}

	duration := time.Since(start)
	logger.Info("Done",
		zap.Duration("duration", duration),
		zap.String("title", video.Title),
	)

	for _, f := range SelectFormats(video.Formats) {
		logger.Info("Format",
			zap.String("id", f.FormatID),
			zap.String("ext", f.Ext),
			zap.String("acodec", f.ACodec),
			zap.String("vcodec", f.VCodec),
			zap.String("resolution", f.Resolution),
		)
	}

	bestVideo := BestVideo(video.Formats)
	bestAudio := BestAudio(video.Formats)

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return errors.Wrap(err, "listen")
	}
	mux := http.NewServeMux()
	videoFile := &ytio.File{
		Path: "/tmp/video.mp4",
	}
	audioFile := &ytio.File{
		Path: "/tmp/audio.m4a",
	}
	mux.Handle("/video.mp4", videoFile)
	mux.Handle("/audio.m4a", audioFile)
	srv := &http.Server{
		Handler: mux,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Serve", zap.Error(err))
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	u := &url.URL{
		Scheme: "http",
		Host:   ln.Addr().String(),
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return downloadChunked(ctx, bestVideo, videoFile, httpClient)
	})
	g.Go(func() error {
		return downloadChunked(ctx, bestAudio, audioFile, httpClient)
	})
	g.Go(func() error {
		stderr := new(bytes.Buffer)
		time.Sleep(time.Second)
		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-i", u.String()+"/video.mp4",
			"-seekable", "1",
			"-i", u.String()+"/audio.m4a",
			"-c:v", "copy",
			"-c:a", "copy",
			"-f", "mp4",
			"-y",
			"-hide_banner",
			"output.mp4",
		)
		cmd.Env = []string{}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		logger.Info("Running",
			zap.String("cmd", cmd.String()),
		)

		if err := cmd.Run(); err != nil {
			return errors.Wrapf(err, "ffmpeg: %s", stderr.String())
		}

		return nil
	})

	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "download")
	}

	return nil
}

func main() {
	ctx := context.Background()
	logger, _ := zap.NewProduction()
	defer func() {
		_ = logger.Sync()
	}()
	ctx = zctx.Base(ctx, logger)
	if err := run(ctx, logger); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
	}
}
