package main

import (
	"encoding/base64"
	"fmt"

	"massdownloader/internal/imageclipboard"
)

// ClipboardSetImage receives PNG pixels captured from the visible viewer
// image/video frame, including local files and authenticated remote media.
func (a *App) ClipboardSetImage(encoded string) error {
	if len(encoded) > base64.StdEncoding.EncodedLen(imageclipboard.MaxPNGBytes) {
		return fmt.Errorf("изображение для буфера обмена слишком большое")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("некорректные данные изображения: %w", err)
	}
	return imageclipboard.SetPNG(data)
}
