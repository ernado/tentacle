package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/go-faster/errors"
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

func run(ctx context.Context) error {
	buf := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	start := time.Now()
	{
		additionalArgs := os.Args[1:]

		// 1. Get JSON video info.
		args := []string{
			"-j",
		}
		args = append(args, additionalArgs...)
		cmd := exec.CommandContext(ctx, "yt-dlp", args...)

		slog.Info("Running",
			slog.String("cmd", cmd.String()),
		)

		cmd.Stdout = buf
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return errors.Wrapf(err, "yt-dlp: %s", stderr.String())
		}
	}

	var video ytdlp.Video
	if err := json.Unmarshal(buf.Bytes(), &video); err != nil {
		return errors.Wrap(err, "unmarshal video info")
	}

	duration := time.Since(start)
	slog.Info("Done",
		slog.Duration("duration", duration),
		slog.String("title", video.Title),
	)

	for _, f := range SelectFormats(video.Formats) {
		slog.Info("Format",
			slog.String("id", f.FormatID),
			slog.String("ext", f.Ext),
			slog.String("acodec", f.ACodec),
			slog.String("vcodec", f.VCodec),
			slog.String("resolution", f.Resolution),
		)
		req, err := http.NewRequestWithContext(ctx, "HEAD", f.URL, nil)
		if err != nil {
			return errors.Wrap(err, "create request")
		}
		for k, v := range f.HTTPHeaders {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return errors.Wrap(err, "do request")
		}
		func() {
			defer func() {
				_ = res.Body.Close()
			}()
			slog.Info("HEAD",
				slog.Int("status", res.StatusCode),
				slog.String("status_text", res.Status),
				slog.Int64("content_length", res.ContentLength),
				slog.String("content_type", res.Header.Get("Content-Type")),
			)
		}()
	}

	bestVideo := BestVideo(video.Formats)
	bestAudio := BestAudio(video.Formats)

	mux := http.NewServeMux()
	copyRequest := func(f ytdlp.Format) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), r.Method, f.URL, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for k := range r.Header {
				req.Header.Set(k, r.Header.Get(k))
			}
			for k, v := range bestVideo.HTTPHeaders {
				req.Header.Set(k, v)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				slog.Error("do", slog.String("error", err.Error()))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() {
				_ = res.Body.Close()
			}()
			for k := range res.Header {
				w.Header().Set(k, res.Header.Get(k))
			}
			w.WriteHeader(res.StatusCode)
			if _, err := io.Copy(w, res.Body); err != nil {
				slog.Error("copy", slog.String("error", err.Error()))
			}
		})
	}
	mux.Handle("/video.mp4", copyRequest(bestVideo))
	mux.Handle("/audio.m4a", copyRequest(bestAudio))
	mux.Handle("/output.mp4",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slog.Info("Got ffmpeg POST request")
			defer func() {
				slog.Info("ffmpeg POST done")
			}()
		}),
	)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second*5)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", slog.String("error", err.Error()))
		}
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", slog.String("error", err.Error()))
		}
	}()

	{
		time.Sleep(time.Second)
		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-i", "http://localhost:8080/video.mp4",
			"-i", "http://localhost:8080/audio.m4a",
			"-c:v", "copy",
			"-c:a", "copy",
			"-f", "mp4",
			"-y",
			"output.mp4",
		)
		cmd.Env = []string{}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		slog.Info("Running",
			slog.String("cmd", cmd.String()),
		)

		if err := cmd.Run(); err != nil {
			return errors.Wrapf(err, "ffmpeg: %s", stderr.String())
		}
	}

	return nil
}

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
	}
}
