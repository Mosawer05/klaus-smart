package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Bilder aus Discord fuer das Modell vorbereiten
// ---------------------------------------------------------------------------

const (
	maxImages      = 3
	maxImageBytes  = 25 << 20
	maxImageSide   = 1536
	maxImagePixels = 1_200_000 // ca. 1200 Bild-Tokens bei Qwen-VL; der Server kappt zusaetzlich
	jpegQuality    = 90
)

// imageAttachments sammelt Bildanhaenge der Nachricht und (falls sie eine
// Antwort ist) der beantworteten Nachricht. "@Klaus was ist das?" als Antwort
// auf ein Bild funktioniert also auch.
func imageAttachments(m *discordgo.Message) []*discordgo.MessageAttachment {
	var out []*discordgo.MessageAttachment
	add := func(msg *discordgo.Message) {
		if msg == nil {
			return
		}
		for _, a := range msg.Attachments {
			if len(out) < maxImages && isImageAttachment(a) {
				out = append(out, a)
			}
		}
		for _, a := range embedImages(msg) {
			if len(out) < maxImages {
				out = append(out, a)
			}
		}
	}
	add(m)
	add(m.ReferencedMessage)
	return out
}

// embedImages: GIFs (Tenor, Giphy) und Bild-Links, die Discord als Vorschau
// einbettet. Klaus bekommt davon das Standbild bzw. das Bild.
func embedImages(m *discordgo.Message) []*discordgo.MessageAttachment {
	var out []*discordgo.MessageAttachment
	for _, e := range m.Embeds {
		if e == nil || (e.Type != discordgo.EmbedTypeGifv && e.Type != discordgo.EmbedTypeImage) {
			continue
		}
		name := "gif"
		if e.Provider != nil && e.Provider.Name != "" {
			name = "gif von " + e.Provider.Name
		}
		switch {
		case e.Thumbnail != nil && e.Thumbnail.URL != "":
			out = append(out, &discordgo.MessageAttachment{Filename: name, URL: e.Thumbnail.URL, ProxyURL: e.Thumbnail.ProxyURL,
				Width: e.Thumbnail.Width, Height: e.Thumbnail.Height, ContentType: "image/png"})
		case e.Image != nil && e.Image.URL != "":
			out = append(out, &discordgo.MessageAttachment{Filename: name, URL: e.Image.URL, ProxyURL: e.Image.ProxyURL,
				Width: e.Image.Width, Height: e.Image.Height, ContentType: "image/png"})
		}
	}
	return out
}

func isImageAttachment(a *discordgo.MessageAttachment) bool {
	if strings.HasPrefix(strings.ToLower(a.ContentType), "image/") {
		return true
	}
	switch strings.ToLower(path.Ext(a.Filename)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// loadImage laedt ein Bild, verkleinert es und liefert eine data:-URL (JPEG).
func loadImage(ctx context.Context, client *http.Client, a *discordgo.MessageAttachment) (string, error) {
	var lastErr error
	for _, u := range imageCandidateURLs(a) {
		img, err := fetchImage(ctx, client, u)
		if err != nil {
			lastErr = err
			continue
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, flattenAndShrink(img), &jpeg.Options{Quality: jpegQuality}); err != nil {
			return "", err
		}
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
	}
	if lastErr == nil {
		lastErr = errors.New("keine URL")
	}
	return "", lastErr
}

// imageCandidateURLs: zuerst Discords Media-Proxy (liefert schon verkleinert
// und als PNG, das klappt auch fuer WebP), dann das Original.
func imageCandidateURLs(a *discordgo.MessageAttachment) []string {
	var urls []string
	if a.ProxyURL != "" {
		if u, err := url.Parse(a.ProxyURL); err == nil {
			q := u.Query()
			q.Set("format", "png")
			if w, h := fitSize(a.Width, a.Height); w > 0 && h > 0 {
				q.Set("width", strconv.Itoa(w))
				q.Set("height", strconv.Itoa(h))
			}
			u.RawQuery = q.Encode()
			urls = append(urls, u.String())
		}
	}
	if a.URL != "" {
		urls = append(urls, a.URL)
	}
	return urls
}

func fetchImage(ctx context.Context, client *http.Client, u string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxImageBytes {
		return nil, errors.New("Bild zu gross")
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("Bildformat nicht lesbar: %w", err)
	}
	return img, nil
}

// fitSize skaliert w x h so, dass beide Grenzen eingehalten werden.
func fitSize(w, h int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	scale := 1.0
	if s := float64(maxImageSide) / float64(max(w, h)); s < scale {
		scale = s
	}
	if s := math.Sqrt(float64(maxImagePixels) / float64(w*h)); s < scale {
		scale = s
	}
	return max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
}

// flattenAndShrink legt das Bild auf weissen Hintergrund (Transparenz wuerde
// in JPEG schwarz) und verkleinert es per Flaechenmittelung.
func flattenAndShrink(src image.Image) image.Image {
	b := src.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Over)

	w, h := fitSize(b.Dx(), b.Dy())
	if w >= b.Dx() && h >= b.Dy() {
		return flat
	}
	return shrink(flat, w, h)
}

func shrink(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := y * sh / h
		y1 := max(y0+1, (y+1)*sh/h)
		for x := 0; x < w; x++ {
			x0 := x * sw / w
			x1 := max(x0+1, (x+1)*sw/w)
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+3]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					n++
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(bl / n)
			dst.Pix[o+3] = 255
		}
	}
	return dst
}
