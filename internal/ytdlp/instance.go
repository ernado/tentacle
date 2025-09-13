package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"

	"github.com/go-faster/errors"
)

type Instance struct {
	CookiesFilePath string
	Proxy           string
}

func (i *Instance) Video(ctx context.Context, uri string) (*Video, error) {
	buf := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	args := []string{
		"-j",
		uri,
	}
	if i.CookiesFilePath != "" {
		args = append(args,
			"--cookies", i.CookiesFilePath,
		)
	}
	if i.Proxy != "" {
		args = append(args,
			"--proxy", i.Proxy,
		)
	}
	cmd := exec.CommandContext(
		ctx, "yt-dlp", args...,
	)
	cmd.Stdout = buf
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "yt-dlp: %s", stderr.String())
	}

	var video Video
	if err := json.Unmarshal(buf.Bytes(), &video); err != nil {
		return nil, err
	}

	return &video, nil
}
