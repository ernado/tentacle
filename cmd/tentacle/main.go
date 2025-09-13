package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/ernado/ff/ffprobe"
	"github.com/ernado/ff/ffrun"
	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/ernado/tentacle/internal/ytio"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func main() {
	app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
		dispatcher := tg.NewUpdateDispatcher()
		opt := telegram.Options{
			UpdateHandler: dispatcher,
			Logger:        lg.Named("gotd"),
		}

		proxyURL := os.Getenv("PROXY_URL")
		cookies := os.Getenv("COOKIES_FILE")

		if err := telegram.BotFromEnvironment(ctx, opt, func(ctx context.Context, client *telegram.Client) error {
			var (
				api    = tg.NewClient(client)
				sender = message.NewSender(api)
				i      = ffrun.New(ffrun.Options{})
			)
			dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
				ctx, cancel := context.WithTimeout(ctx, time.Minute*30)
				defer cancel()

				m, ok := u.Message.(*tg.Message)
				if !ok || m.Out {
					return nil
				}

				var (
					reply  = sender.Reply(e, u)
					lg     = lg.With(zap.Int("msg_id", m.ID))
					answer = sender.Answer(e, u)
				)

				uri, err := url.Parse(m.Message)
				if err != nil {
					return errors.Wrap(err, "parse url")
				}

				httpClient, err := ytdlp.NewHTTPClientWithProxy(proxyURL)
				if err != nil {
					return errors.Wrap(err, "create http client")
				}

				instance := &ytdlp.Instance{
					CookiesFilePath: cookies,
					Proxy:           proxyURL,
				}
				start := time.Now()

				if _, err := answer.Textf(ctx, "Getting info..."); err != nil {
					return errors.Wrap(err, "send answer")
				}

				video, err := instance.Video(ctx, uri.String())
				if err != nil {
					return errors.Wrap(err, "fetch video info")
				}

				duration := time.Since(start)
				lg.Info("Done",
					zap.Duration("duration", duration),
					zap.String("title", video.Title),
				)

				for _, f := range ytdlp.SelectFormats(video.Formats) {
					lg.Info("Format",
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
						lg.Error("Serve", zap.Error(err))
					}
				}()
				go func() {
					<-ctx.Done()
					_ = srv.Close()
				}()
				serveURI := &url.URL{
					Scheme: "http",
					Host:   ln.Addr().String(),
				}

				if _, err := answer.Textf(ctx, "Downloading..."); err != nil {
					return errors.Wrap(err, "send answer")
				}

				tempFile, err := os.CreateTemp("", "video-*.mp4")
				if err != nil {
					return errors.Wrap(err, "create temp file")
				}
				tempPath := tempFile.Name()
				_ = tempFile.Close()

				done := make(chan struct{})
				g, gCtx := errgroup.WithContext(ctx)
				g.Go(func() error {
					return ytdlp.DownloadChunked(gCtx, bestVideo, videoFile, httpClient)
				})
				g.Go(func() error {
					return ytdlp.DownloadChunked(gCtx, bestAudio, audioFile, httpClient)
				})

				outputPath := tempPath
				g.Go(func() error {
					defer close(done)
					stderr := new(bytes.Buffer)
					defer lg.Info("ffmpeg done")
					time.Sleep(time.Second)
					cmd := exec.CommandContext(gCtx, "ffmpeg",
						"-i", serveURI.String()+"/video.mp4",
						"-seekable", "1",
						"-i", serveURI.String()+"/audio.m4a",
						"-c:v", "copy",
						"-c:a", "copy",
						"-f", "mp4",
						"-y",
						"-hide_banner",
						outputPath,
					)
					cmd.Env = []string{}
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					lg.Info("Running",
						zap.String("cmd", cmd.String()),
					)

					if err := cmd.Run(); err != nil {
						return errors.Wrapf(err, "ffmpeg: %s", stderr.String())
					}

					return nil
				})
				g.Go(func() error {
					// Upload part by part, streaming video from disk.
					id, err := crypto.RandInt64(crypto.DefaultRand())
					if err != nil {
						return errors.Wrap(err, "generate id")
					}

					stat, err := os.Stat(outputPath)
					if err != nil {
						return errors.Wrap(err, "stat")
					}

					lg.Info("Uploading",
						zap.Int64("id", id),
						zap.Int64("size", stat.Size()),
					)

					const partSize = uploader.MaximumPartSize
					totalParts := -1
					if err := ytio.StreamFile(gCtx, done, outputPath, partSize, func(part ytio.StreamPart) error {
						if len(part.Data) == 0 {
							lg.Info("Skip empty part")
							return nil
						} else {
							lg.Info("Uploading part",
								zap.Int("index", part.Index),
								zap.Int("size", len(part.Data)),
								zap.Int64("offset", part.Offset),
							)
						}
						if part.Last {
							totalParts = part.Index
						}
						_, err := api.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{
							FileID:         id,
							FilePart:       part.Index,
							FileTotalParts: totalParts,
							Bytes:          part.Data,
						})
						if err != nil {
							return errors.Wrap(err, "upload part")
						}

						return nil
					}); err != nil {
						return errors.Wrap(err, "stream file")
					}

					// Upload moov atom.
					data := make([]byte, partSize)
					file, err := os.Open(outputPath)
					if err != nil {
						return errors.Wrap(err, "open file")
					}
					defer func() {
						_ = file.Close()
					}()

					if _, err := io.ReadFull(file, data); err != nil {
						return errors.Wrap(err, "read full")
					}

					if _, err = api.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{
						FileID:         id,
						FilePart:       1,
						FileTotalParts: totalParts,
						Bytes:          data,
					}); err != nil {
						return errors.Wrap(err, "upload moov")
					}

					return nil
				})

				if err := g.Wait(); err != nil {
					return errors.Wrap(err, "download")
				}

				if _, err := answer.Textf(ctx, "Uploading..."); err != nil {
					return errors.Wrap(err, "send answer")
				}

				// Pick first frame of video as thumbnail.
				previewPath := "/tmp/preview.jpg"
				if err := i.Run(ctx, ffrun.RunOptions{
					Input:  outputPath,
					Output: previewPath,
					Args: []string{
						"-vf", "scale=-2:720",
						"-frames:v", "1",
					},
				}); err != nil {
					return errors.Wrap(err, "preview")
				}

				screen, err := uploader.NewUploader(api).
					FromPath(ctx, previewPath)
				if err != nil {
					return errors.Wrap(err, "upload")
				}

				stat, err := os.Stat(outputPath)
				if err != nil {
					return errors.Wrap(err, "stat")
				}
				partSize := 128 * 1024
				size := int(stat.Size())
				const (
					kb int = 1024
					mb     = kb * 1024
					gb     = mb * 1024
				)
				if size > 100*mb {
					partSize = uploader.MaximumPartSize
				}
				if size > 4*gb {
					return errors.Wrapf(err, "%s: file too big", outputPath)
				}

				probe, err := i.Probe(ctx, outputPath)
				if err != nil {
					return err
				}
				summary, err := ffprobe.ParseSummary(probe)
				if err != nil {
					return errors.Wrap(err, "summary")
				}

				lg.Info("Got summary",
					zap.Duration("duration", summary.Duration),
					zap.Int("width", summary.Width),
					zap.Int("height", summary.Height),
				)

				const threads = 3
				upload, err := uploader.NewUploader(api).
					WithThreads(threads).
					WithPartSize(partSize).
					FromPath(ctx, outputPath)
				if err != nil {
					return fmt.Errorf("upload: %w", err)
				}
				lg.Info("Uploaded")
				if _, err := reply.Media(ctx,
					message.UploadedDocument(upload).
						Filename("output.mp4").
						MIME("video/mp4").
						Thumb(screen).
						Video().
						SupportsStreaming().
						Resolution(summary.Width, summary.Height).
						Duration(summary.Duration),
				); err != nil {
					return err
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
