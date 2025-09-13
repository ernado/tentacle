package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
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

func main() {
	app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
		dispatcher := tg.NewUpdateDispatcher()
		opt := telegram.Options{
			UpdateHandler: dispatcher,
			Logger:        lg.Named("gotd"),
		}

		if err := telegram.BotFromEnvironment(ctx, opt, func(ctx context.Context, client *telegram.Client) error {
			var (
				api    = tg.NewClient(client)
				sender = message.NewSender(api)
			)
			dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
				ctx, cancel := context.WithTimeout(ctx, time.Minute*30)
				defer cancel()

				m, ok := u.Message.(*tg.Message)
				if !ok || m.Out {
					return nil
				}

				lg := lg.With(zap.Int("msg_id", m.ID))

				uri, err := url.Parse(m.Message)
				if err != nil {
					return errors.Wrap(err, "parse url")
				}

				buf := new(bytes.Buffer)
				stderr := new(bytes.Buffer)
				{
					additionalArgs := os.Args[1:]

					// 1. Get JSON video info.
					args := []string{
						"-j",
						"--cookies-from-browser", "firefox",
						uri.String(),
					}
					args = append(args, additionalArgs...)
					cmd := exec.CommandContext(ctx, "yt-dlp", args...)

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
						_, _ = io.Copy(w, res.Body)
					})
				}
				mux.Handle("/video.mp4", copyRequest(bestVideo))
				mux.Handle("/audio.m4a", copyRequest(bestAudio))

				ln, err := net.Listen("tcp", "localhost:0")
				if err != nil {
					return errors.Wrap(err, "listen")
				}

				srv := &http.Server{
					Handler: mux,
				}

				go func() {
					<-ctx.Done()
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second*5)
					defer shutdownCancel()
					if err := srv.Shutdown(shutdownCtx); err != nil {
						lg.Error("shutdown", zap.Error(err))
					}
				}()
				go func() {
					if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
						lg.Error("serve", zap.Error(err))
					}
				}()

				outputFileName := fmt.Sprintf("%d.mp4", m.ID)
				inputHTTPAddress := fmt.Sprintf("http://%s", ln.Addr().String())

				{

					cmd := exec.CommandContext(ctx, "ffmpeg",
						"-i", inputHTTPAddress+"/video.mp4",
						"-i", inputHTTPAddress+"/audio.m4a",
						"-c:v", "copy",
						"-c:a", "copy",
						"-f", "mp4",
						"-y",
						outputFileName,
					)
					cmd.Env = []string{}
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr

					if err := cmd.Run(); err != nil {
						return errors.Wrapf(err, "ffmpeg: %s", stderr.String())
					}
				}

				answer := sender.Answer(e, u)

				if _, err := answer.Textf(ctx, "Uploading..."); err != nil {
					return errors.Wrap(err, "send answer")
				}

				return nil
			})

			return nil
		}, telegram.RunUntilCanceled); err != nil {
			panic(err)
		}
		return nil
	})
}
