package browsercontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"time"
)

// Each preview is an independent, input-free snapshot. No viewer session or
// framebuffer history survives this request, and all pixels stay in memory.
func (c *Client) dockerPreview(ctx context.Context) (json.RawMessage, error) {
	if !c.previewMu.TryLock() {
		return nil, errors.New("Docker preview busy")
	}
	defer c.previewMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	conn, err := dialDockerVNC(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	pixels, err := captureDesktop(conn)
	if err != nil {
		return nil, fmt.Errorf("Docker preview failed: %w", err)
	}
	// A preview needs less resolution than an interactive desktop.
	if pixels.Bounds().Dx() > 1280 {
		w, h := pixels.Bounds().Dx(), pixels.Bounds().Dy()
		small := image.NewRGBA(image.Rect(0, 0, 1280, max(1, h*1280/w)))
		for y := 0; y < small.Bounds().Dy(); y++ {
			for x := 0; x < 1280; x++ {
				small.SetRGBA(x, y, pixels.RGBAAt(x*w/1280, y*h/small.Bounds().Dy()))
			}
		}
		pixels = small
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, pixels, &jpeg.Options{Quality: 60}); err != nil {
		return nil, err
	}
	if jpegBytes.Len() > 700*1024 {
		return nil, errors.New("Docker preview image too large")
	}
	return json.Marshal(struct {
		Frame  string `json:"frame"`
		Image  string `json:"image"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}{fmt.Sprintf("preview-%d", time.Now().UnixNano()), base64.StdEncoding.EncodeToString(jpegBytes.Bytes()), pixels.Bounds().Dx(), pixels.Bounds().Dy()})
}

func captureDesktop(conn net.Conn) (*image.RGBA, error) {
	// Shared ClientInit never evicts an existing interactive viewer.
	if _, err := conn.Write([]byte{1}); err != nil {
		return nil, err
	}
	init := make([]byte, 24)
	if _, err := io.ReadFull(conn, init); err != nil {
		return nil, err
	}
	w, h := int(binary.BigEndian.Uint16(init)), int(binary.BigEndian.Uint16(init[2:]))
	nameSize := binary.BigEndian.Uint32(init[20:])
	if w == 0 || h == 0 || w > 4096 || h > 4096 || w*h > 8_388_608 || nameSize > 4096 {
		return nil, errors.New("unsupported desktop size")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(nameSize)); err != nil {
		return nil, err
	}
	// Request little-endian 32-bit RGB and only Raw encoding, followed by a
	// non-incremental full update. No keyboard, pointer or clipboard messages.
	setup := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0,
		2, 0, 0, 1, 0, 0, 0, 0,
		3, 0, 0, 0, 0, 0, byte(w >> 8), byte(w), byte(h >> 8), byte(h)}
	if _, err := conn.Write(setup); err != nil {
		return nil, err
	}
	pixels := image.NewRGBA(image.Rect(0, 0, w, h))
	covered := make([]bool, w*h)
	remaining := w * h
	budget := w*h*8 + 65536
	for messages := 0; messages < 1024 && remaining > 0; messages++ {
		typ := make([]byte, 1)
		if _, err := io.ReadFull(conn, typ); err != nil {
			return nil, err
		}
		switch typ[0] {
		case 0:
			if err := readPreviewUpdate(conn, pixels, covered, &remaining, &budget); err != nil {
				return nil, err
			}
		case 2: // Bell has no payload.
		case 3: // Discard bounded clipboard announcements without retaining text.
			header := make([]byte, 7)
			if _, err := io.ReadFull(conn, header); err != nil {
				return nil, err
			}
			n := binary.BigEndian.Uint32(header[3:])
			if n > 65536 || int(n) > budget {
				return nil, errors.New("preview message too large")
			}
			budget -= int(n)
			if _, err := io.CopyN(io.Discard, conn, int64(n)); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("unsupported preview message")
		}
	}
	if remaining != 0 {
		return nil, errors.New("incomplete desktop preview")
	}
	return pixels, nil
}

func readPreviewUpdate(r io.Reader, pixels *image.RGBA, covered []bool, remaining, budget *int) error {
	header := make([]byte, 3)
	if _, err := io.ReadFull(r, header); err != nil {
		return err
	}
	count := int(binary.BigEndian.Uint16(header[1:]))
	if count > 4096 {
		return errors.New("too many preview rectangles")
	}
	for i := 0; i < count; i++ {
		rect := make([]byte, 12)
		if _, err := io.ReadFull(r, rect); err != nil {
			return err
		}
		x, y := int(binary.BigEndian.Uint16(rect)), int(binary.BigEndian.Uint16(rect[2:]))
		w, h := int(binary.BigEndian.Uint16(rect[4:])), int(binary.BigEndian.Uint16(rect[6:]))
		if binary.BigEndian.Uint32(rect[8:]) != 0 || w == 0 || h == 0 || x+w > pixels.Bounds().Dx() || y+h > pixels.Bounds().Dy() || w*h*4 > *budget {
			return errors.New("invalid preview rectangle")
		}
		*budget -= w * h * 4
		for row := y; row < y+h; row++ {
			offset := pixels.PixOffset(x, row)
			line := pixels.Pix[offset : offset+w*4]
			if _, err := io.ReadFull(r, line); err != nil {
				return err
			}
			for col := 0; col < w; col++ {
				line[col*4+3] = 255
				index := row*pixels.Bounds().Dx() + x + col
				if !covered[index] {
					covered[index] = true
					*remaining--
				}
			}
		}
	}
	return nil
}
