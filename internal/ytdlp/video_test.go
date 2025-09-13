package ytdlp

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

//go:embed _testdata/video.json
var videeoExample []byte

func TestVideo(t *testing.T) {
	var video Video
	require.NoError(t, json.Unmarshal(videeoExample, &video))
}
