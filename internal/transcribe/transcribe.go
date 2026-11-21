package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Probe returns the audio duration in seconds as reported by ffprobe.
func Probe(ctx context.Context, path string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

// ToWAV converts to 16 kHz mono 16-bit PCM, the input format Meetily feeds Whisper.
func ToWAV(ctx context.Context, in, out string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-y", "-i", in, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type Client struct {
	URL string
}

// Whisper posts the WAV to whisper.cpp's /inference with Indonesian fixed (BR-004).
// The timeout is twice the audio length, at least 10 minutes (FR-006).
func (c *Client) Whisper(ctx context.Context, wavPath string, durationSec float64) ([]Segment, error) {
	timeout := time.Duration(2*durationSec) * time.Second
	if timeout < 10*time.Minute {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	f, err := os.Open(wavPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, v := range map[string]string{"language": "id", "response_format": "verbose_json", "temperature": "0"} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("file", filepath.Base(wavPath))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/inference", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whisper status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out struct {
		Segments []Segment `json:"segments"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode whisper response: %w", err)
	}
	return out.Segments, nil
}
