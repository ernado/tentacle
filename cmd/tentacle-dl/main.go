package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/ernado/tentacle/internal/ytio"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

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

	httpClient, err := ytdlp.NewHTTPClientWithProxy(arg.ProxyURL)
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

	for _, f := range ytdlp.SelectFormats(video.Formats) {
		logger.Info("Format",
			zap.String("id", f.FormatID),
			zap.String("ext", f.Ext),
			zap.String("acodec", f.ACodec),
			zap.String("vcodec", f.VCodec),
			zap.String("resolution", f.Resolution),
		)
	}

	bestVideo := ytdlp.BestVideo(video.Formats)
	bestAudio := ytdlp.BestAudio(video.Formats)

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
		return ytdlp.DownloadChunked(ctx, bestVideo, videoFile, httpClient)
	})
	g.Go(func() error {
		return ytdlp.DownloadChunked(ctx, bestAudio, audioFile, httpClient)
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
