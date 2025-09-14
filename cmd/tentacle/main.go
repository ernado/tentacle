package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/ernado/ff/ffprobe"
	"github.com/ernado/ff/ffrun"
	"github.com/ernado/tentacle/internal/tgpool"
	"github.com/ernado/tentacle/internal/ytdlp"
	"github.com/ernado/tentacle/internal/ytio"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func createTempFile(pattern string) (string, error) {
	tempFile, err := os.CreateTemp("", "tentacle-"+pattern)
	if err != nil {
		return "", errors.Wrap(err, "create temp file")
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		return "", errors.Wrap(err, "close temp file")
	}

	return tempPath, nil
}

const (
	EnvBotToken        = "BOT_TOKEN"
	EnvApplicationID   = "APP_ID"
	EnvApplicationHash = "APP_HASH"
)

var _ uploader.Progress = (*ZapProgressHandler)(nil)

type ZapProgressHandler struct {
	Logger *zap.Logger
}

func (z ZapProgressHandler) Chunk(_ context.Context, state uploader.ProgressState) error {
	z.Logger.Info("Upload progress",
		zap.Int64("id", state.ID),
		zap.String("name", state.Name),
		zap.Int64("total", state.Total),
		zap.Int("part", state.Part),
	)
	return nil
}

func main() {
	app.Run(func(ctx context.Context, logger *zap.Logger, t *app.Telemetry) error {
		botToken := os.Getenv(EnvBotToken)
		if botToken == "" {
			return errors.New("BOT_TOKEN is empty")
		}
		appID, err := strconv.Atoi(os.Getenv(EnvApplicationID))
		if err != nil {
			return errors.Wrap(err, "parse APP_ID")
		}
		appHash := os.Getenv(EnvApplicationHash)
		if appHash == "" {
			return errors.New("APP_HASH is empty")
		}

		dispatcher := tg.NewUpdateDispatcher()

		proxyURL := os.Getenv("PROXY_URL")
		cookies := os.Getenv("COOKIES_FILE")

		// Create pool of uploaders.
		pool := tgpool.New()

		g, ctx := errgroup.WithContext(ctx)

		const poolSize = 0

		for i := 0; i < poolSize; i++ {
			waiter := floodwait.NewWaiter()

			g.Go(func() error {
				filePath := "/tmp/tg-session-" + strconv.Itoa(i) + ".json"
				client := telegram.NewClient(appID, appHash, telegram.Options{
					NoUpdates:      true,
					Logger:         logger.Named("tg").With(zap.Int("shard", i)),
					TracerProvider: t.TracerProvider(),
					SessionStorage: &session.FileStorage{
						Path: filePath,
					},
					Middlewares: []telegram.Middleware{waiter},
				})
				return waiter.Run(ctx, func(ctx context.Context) error {
					if err := client.Run(ctx, func(ctx context.Context) error {
						if _, err := client.Auth().Bot(ctx, botToken); err != nil {
							return errors.Wrap(err, "bot auth")
						}
						pool.Add(client)
						<-ctx.Done()
						return nil
					}); err != nil {
						return errors.Wrap(err, "run client")
					}

					return nil
				})
			})
		}
		opt := telegram.Options{
			UpdateHandler: dispatcher,
			Logger:        logger.Named("gotd"),
		}
		g.Go(func() error {
			if err := telegram.BotFromEnvironment(ctx, opt, func(ctx context.Context, client *telegram.Client) error {
				pool.Add(client)
				var (
					api       = tg.NewClient(client)
					pooledAPI = tg.NewClient(pool)
					sender    = message.NewSender(api)
					i         = ffrun.New(ffrun.Options{})
					up        = uploader.NewUploader(pooledAPI).
							WithPartSize(uploader.MaximumPartSize).
							WithThreads(3).
							WithProgress(ZapProgressHandler{Logger: logger.Named("uploader")})
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
						lg     = logger.With(zap.Int("msg_id", m.ID))
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
					videoPath, err := createTempFile("video-*.mp4")
					if err != nil {
						return errors.Wrap(err, "create video temp file")
					}
					videoFile := &ytio.File{
						Path: videoPath,
					}
					defer func() { _ = os.Remove(videoPath) }()

					bestAudio := ytdlp.BestAudio(video.Formats)
					audioPath, err := createTempFile("audio-*.m4a")
					if err != nil {
						return errors.Wrap(err, "create audio temp file")
					}
					audioFile := &ytio.File{
						Path: audioPath,
					}
					defer func() { _ = os.Remove(audioPath) }()

					if _, err := answer.Text(ctx, "Downloading..."); err != nil {
						return errors.Wrap(err, "send answer")
					}

					g, gCtx := errgroup.WithContext(ctx)
					g.Go(func() error {
						if err := ytdlp.DownloadChunked(gCtx, bestVideo, videoFile, httpClient); err != nil {
							lg.Error("Video download error", zap.Error(err))
							return errors.Wrap(err, "download video")
						}

						return nil
					})
					g.Go(func() error {
						if err := ytdlp.DownloadChunked(gCtx, bestAudio, audioFile, httpClient); err != nil {
							lg.Error("Audio download error", zap.Error(err))
							return errors.Wrap(err, "download audio")
						}

						return nil
					})

					if err := g.Wait(); err != nil {
						return errors.Wrap(err, "download")
					}

					if _, err := answer.Textf(ctx, "Uploading..."); err != nil {
						return errors.Wrap(err, "send answer")
					}

					outputPath, err := createTempFile("output-*.mp4")
					if err != nil {
						return errors.Wrap(err, "create output temp file")
					}

					// TODO: Use ff.
					ffmpegErrorStream := new(bytes.Buffer)
					ffmpegCommand := exec.CommandContext(ctx,
						"ffmpeg",
						"-i", videoFile.Path,
						"-i", audioFile.Path,
						"-c:v", "copy",
						"-c:a", "copy",
						"-f", "mp4",
						"-movflags", "faststart",
						"-y",
						"-hide_banner",
						outputPath,
					)
					ffmpegCommand.Env = []string{}
					ffmpegCommand.Stdout = os.Stdout // TODO: drop
					ffmpegCommand.Stderr = ffmpegErrorStream
					lg.Info("Running",
						zap.String("ffmpegCommand", ffmpegCommand.String()),
					)

					if err := ffmpegCommand.Run(); err != nil {
						return errors.Wrapf(err, "ffmpeg: %s", ffmpegErrorStream.String())
					}

					// Pick first frame of video as thumbnail.
					previewPath, err := createTempFile("preview-*.jpg")
					if err != nil {
						return errors.Wrap(err, "create preview temp file")
					}

					if err := i.Run(ctx, ffrun.RunOptions{
						Input:  outputPath,
						Output: previewPath,
						Args: []string{
							"-frames:v", "1",
						},
					}); err != nil {
						return errors.Wrap(err, "preview")
					}

					summary, err := i.Probe(ctx, outputPath)
					if err != nil {
						return errors.Wrap(err, "probe for output")
					}
					parsedSummary, err := ffprobe.ParseSummary(summary)
					if err != nil {
						return errors.Wrap(err, "parse summary for output")
					}

					thumbnail, err := up.FromPath(ctx, previewPath)
					if err != nil {
						return errors.Wrap(err, "upload")
					}

					lg.Info("Got summary",
						zap.Duration("duration", parsedSummary.Duration),
						zap.Int("width", parsedSummary.Width),
						zap.Int("height", parsedSummary.Height),
					)

					outputFile, err := os.Open(outputPath)
					if err != nil {
						return errors.Wrap(err, "open output")
					}
					defer func() { _ = outputFile.Close() }()

					stat, err := outputFile.Stat()
					if err != nil {
						return errors.Wrap(err, "stat output")
					}

					inputClass, err := up.
						Upload(ctx, uploader.NewUpload("output.mp4", outputFile, stat.Size()))
					if err != nil {
						return errors.Wrap(err, "upload")
					}

					if err != nil {
						return errors.Wrap(err, "upload")
					}
					lg.Info("Uploaded")

					uploadedDocument := message.UploadedDocument(inputClass).
						Filename("output.mp4").
						MIME("video/mp4").
						Thumb(thumbnail).
						Video().
						Duration(parsedSummary.Duration).
						Resolution(parsedSummary.Width, parsedSummary.Height).
						SupportsStreaming()

					if _, err := reply.Media(ctx, uploadedDocument); err != nil {
						return err
					}

					return nil
				})

				return nil
			}, telegram.RunUntilCanceled); err != nil {
				return errors.Wrap(err, "run bot")
			}

			return nil
		})

		return g.Wait()
	})
}
