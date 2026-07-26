package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"strings"

	sonic "github.com/bytedance/sonic"
)

const maxImageGuardDecodeBytes = 64 << 20

// shrinkRequestImages bounds inline image data URLs. It returns the original
// bytes when nothing changes so same-protocol requests are only rewritten after
// a concrete upstream 413.
func shrinkRequestImages(body []byte, maxBytes, maxDimension int) ([]byte, bool) {
	var root any
	if sonic.Unmarshal(body, &root) != nil {
		return body, false
	}
	changed := shrinkImageNode(root, maxBytes, maxDimension)
	if !changed {
		return body, false
	}
	out, err := sonic.Marshal(root)
	if err != nil {
		return body, false
	}
	return out, true
}

func shrinkImageNode(node any, maxBytes, maxDimension int) bool {
	changed := false
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if text, ok := child.(string); ok {
				if replacement, did := shrinkImageDataURL(text, maxBytes, maxDimension); did {
					value[key] = replacement
					changed = true
				}
				continue
			}
			changed = shrinkImageNode(child, maxBytes, maxDimension) || changed
		}
	case []any:
		for i, child := range value {
			if text, ok := child.(string); ok {
				if replacement, did := shrinkImageDataURL(text, maxBytes, maxDimension); did {
					value[i] = replacement
					changed = true
				}
				continue
			}
			changed = shrinkImageNode(child, maxBytes, maxDimension) || changed
		}
	}
	return changed
}

func shrinkImageDataURL(value string, maxBytes, maxDimension int) (string, bool) {
	if !strings.HasPrefix(value, "data:image/") {
		return value, false
	}
	_, encoded, ok := parseDataURL(value)
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maxImageGuardDecodeBytes) {
		return value, false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return value, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return value, false
	}
	if len(raw) <= maxBytes && cfg.Width <= maxDimension && cfg.Height <= maxDimension {
		return value, false
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return value, false
	}
	width, height := cfg.Width, cfg.Height
	if width > maxDimension || height > maxDimension {
		scale := float64(maxDimension) / float64(max(width, height))
		width = max(1, int(float64(width)*scale))
		height = max(1, int(float64(height)*scale))
	}
	for {
		dst := resizeImageNearest(src, width, height)
		for quality := 85; quality >= 35; quality -= 10 {
			var compressed bytes.Buffer
			if jpeg.Encode(&compressed, dst, &jpeg.Options{Quality: quality}) != nil {
				return value, false
			}
			if compressed.Len() <= maxBytes || quality == 35 {
				if compressed.Len() > maxBytes && width > 256 && height > 256 {
					width = max(256, width*3/4)
					height = max(256, height*3/4)
					break
				}
				return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(compressed.Bytes()), true
			}
		}
	}
}

func resizeImageNearest(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	bounds := src.Bounds()
	srcWidth, srcHeight := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		sy := bounds.Min.Y + y*srcHeight/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*srcWidth/width
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}
