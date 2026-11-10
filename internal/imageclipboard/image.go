// Package imageclipboard puts decoded viewer pixels on the native clipboard.
package imageclipboard

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"image/png"
)

// Keep base64 RPC payloads below the feed bridge's 32 MiB request limit.
const MaxPNGBytes = 20 << 20
const maxPixels = 32 << 20

// clipboardDIBs builds an alpha-preserving DIBV5 and a white-composited DIB for
// older applications that ignore alpha. Both store top-down BGRA pixels.
func clipboardDIBs(data []byte) (v5, legacy []byte, err error) {
	if len(data) == 0 || len(data) > MaxPNGBytes {
		return nil, nil, fmt.Errorf("недопустимый размер PNG для буфера обмена")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось прочитать PNG: %w", err)
	}
	w, h := config.Width, config.Height
	if w <= 0 || h <= 0 || int64(w)*int64(h) > maxPixels {
		return nil, nil, fmt.Errorf("недопустимые размеры изображения")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось декодировать PNG: %w", err)
	}
	size := w * h * 4
	v5, legacy = make([]byte, 124+size), make([]byte, 40+size)
	for _, dib := range [][]byte{v5, legacy} {
		binary.LittleEndian.PutUint32(dib[0:4], uint32(len(dib)-size))
		binary.LittleEndian.PutUint32(dib[4:8], uint32(w))
		binary.LittleEndian.PutUint32(dib[8:12], uint32(-int32(h)))
		binary.LittleEndian.PutUint16(dib[12:14], 1)
		binary.LittleEndian.PutUint16(dib[14:16], 32)
		binary.LittleEndian.PutUint32(dib[20:24], uint32(size))
	}
	binary.LittleEndian.PutUint32(v5[16:20], 3) // BI_BITFIELDS
	binary.LittleEndian.PutUint32(v5[40:44], 0x00ff0000)
	binary.LittleEndian.PutUint32(v5[44:48], 0x0000ff00)
	binary.LittleEndian.PutUint32(v5[48:52], 0x000000ff)
	binary.LittleEndian.PutUint32(v5[52:56], 0xff000000)
	binary.LittleEndian.PutUint32(v5[56:60], 0x73524742) // LCS_sRGB
	binary.LittleEndian.PutUint32(v5[108:112], 4)        // LCS_GM_IMAGES
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixel := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			i := (y*w + x) * 4
			copy(v5[124+i:128+i], []byte{pixel.B, pixel.G, pixel.R, pixel.A})
			white := func(c uint8) uint8 { return uint8((uint32(c)*uint32(pixel.A) + 255*uint32(255-pixel.A) + 127) / 255) }
			copy(legacy[40+i:44+i], []byte{white(pixel.B), white(pixel.G), white(pixel.R), 255})
		}
	}
	return v5, legacy, nil
}
